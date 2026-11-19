package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	metricsCredential  = "metrics-token-abcdefghijklmnopqrstuvwxyz"
	operatorCredential = "operator-token-abcdefghijklmnopqrstuvwxyz"
	scalerCredential   = "scaler-token-abcdefghijklmnopqrstuvwxyz"
)

func metricsRequest(handler http.Handler, method, path, credential, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if credential != "" {
		r.Header.Set("Authorization", "Bearer "+credential)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestMetricsOperatorAccess(t *testing.T) {
	for _, configured := range []string{"", metricsCredential} {
		h := &HTTPServer{OperatorToken: operatorCredential, ScalerToken: scalerCredential, MetricsToken: configured}
		handler := h.Handler()
		for _, candidate := range []string{"", metricsCredential, operatorCredential, scalerCredential,
			strings.Repeat("ab", 32), strings.Repeat("cd", 32)} {
			w := metricsRequest(handler, "GET", "/metrics", candidate, "")
			if candidate == operatorCredential || (configured != "" && candidate == configured) {
				if w.Code != 200 || w.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
					t.Fatalf("metrics status %d, headers %v", w.Code, w.Header())
				}
			} else {
				refusal := metricsRequest(handler, "POST", "/v1/tenants", candidate, "")
				if w.Code != 401 || w.Code != refusal.Code || w.Body.String() != refusal.Body.String() {
					t.Fatalf("metrics refusal %d %q, operator refusal %d %q", w.Code, w.Body, refusal.Code, refusal.Body)
				}
			}
		}
		if configured != "" && metricsRequest(handler, "POST", "/v1/tenants", configured, "").Code != 401 {
			t.Fatal("metrics-only bearer reached an operator route")
		}
	}
}

func metricsScrape(t *testing.T, handler http.Handler) string {
	t.Helper()
	w := metricsRequest(handler, "GET", "/metrics", operatorCredential, "")
	if w.Code != 200 {
		t.Fatalf("scrape status %d: %s", w.Code, w.Body)
	}
	return w.Body.String()
}

var expositionSample = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[a-zA-Z_][a-zA-Z0-9_]*="(?:[^"\\]|\\[\\"n])*"(?:,[a-zA-Z_][a-zA-Z0-9_]*="(?:[^"\\]|\\[\\"n])*")*\})? ([^ ]+)$`)

func parseMetrics(t *testing.T, body string) map[string]float64 {
	t.Helper()
	help := make(map[string]bool)
	types := make(map[string]string)
	samples := make(map[string]float64)
	var families, series []string
	var family string
	for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		if strings.HasPrefix(line, "# ") {
			fields := strings.SplitN(line, " ", 4)
			if len(fields) != 4 || fields[3] == "" {
				t.Fatalf("invalid metadata %q", line)
			}
			name := fields[2]
			switch fields[1] {
			case "HELP":
				if help[name] || !slices.IsSorted(series) {
					t.Fatalf("invalid family order at %q", line)
				}
				family, series = name, nil
				families = append(families, name)
				help[name] = true
			case "TYPE":
				if name != family || types[name] != "" || (fields[3] != "gauge" && fields[3] != "counter") {
					t.Fatalf("invalid type %q", line)
				}
				types[name] = fields[3]
			default:
				t.Fatalf("unknown metadata %q", line)
			}
			continue
		}
		match := expositionSample.FindStringSubmatch(line)
		if match == nil || match[1] != family || !help[family] || types[family] == "" {
			t.Fatalf("invalid sample %q", line)
		}
		key := match[1] + match[2]
		value, err := strconv.ParseFloat(match[3], 64)
		if _, exists := samples[key]; err != nil || exists {
			t.Fatalf("invalid or duplicate sample %q", line)
		}
		samples[key] = value
		series = append(series, key)
	}
	if !slices.IsSorted(families) || !slices.IsSorted(series) || len(help) != len(types) {
		t.Fatal("unsorted or incomplete exposition")
	}
	return samples
}

func assertMetric(t *testing.T, samples map[string]float64, name string, want float64) {
	t.Helper()
	if value, ok := samples[name]; !ok || value != want {
		t.Errorf("%s = %v (present: %t), want %v", name, value, ok, want)
	}
}

func TestMetricsFormatAndCache(t *testing.T) {
	now := time.Unix(0, 0)
	calls := 0
	h := &HTTPServer{
		OperatorToken: operatorCredential,
		metricsNow:    func() time.Time { return now },
		readMetrics: func(context.Context) (StoreMetrics, error) {
			calls++
			if calls == 2 {
				return StoreMetrics{}, errors.New("database unavailable")
			}
			return StoreMetrics{
				Jobs: map[string]int64{"queued": 2}, OldestQueuedSeconds: 42.5,
				Tenants: 3, Projects: 4, SchemaVersion: 7,
			}, nil
		},
	}
	h.eventStreams.acquire("first")
	h.eventStreams.acquire("first")
	h.eventStreams.acquire("second")
	h.eventStreams.release("first")
	handler := h.Handler()
	first := metricsScrape(t, handler)
	samples := parseMetrics(t, first)
	for name, want := range map[string]float64{
		`hrn_build_info{version="` + escapeMetricsLabel(metricsBuildVersion) + `"}`: 1,
		"hrn_event_streams_open": 2, "hrn_jobs_oldest_queued_seconds": 42.5,
		"hrn_tenants": 3, "hrn_projects": 4, "hrn_schema_version": 7, "hrn_metrics_database_up": 1,
		`hrn_submissions_refused_total{reason="cost_limit"}`:    0,
		`hrn_submissions_refused_total{reason="tenant_budget"}`: 0,
		`hrn_oidc_tokens_total{result="accepted"}`:              0,
		`hrn_oidc_tokens_total{result="refused"}`:               0,
		`hrn_oidc_tokens_total{result="unavailable"}`:           0,
	} {
		assertMetric(t, samples, name, want)
	}
	for _, state := range strings.Fields("admitted canceled failed queued recovery_required running succeeded waiting_approval") {
		want := float64(0)
		if state == "queued" {
			want = 2
		}
		assertMetric(t, samples, `hrn_jobs{state="`+state+`"}`, want)
	}
	now = now.Add(4 * time.Second)
	second := metricsScrape(t, handler)
	const scrapeSeries = `hrn_http_requests_total{route="GET /metrics",method="GET",code="200"}`
	assertMetric(t, parseMetrics(t, second), scrapeSeries, 1)
	if calls != 1 || strings.Replace(second, scrapeSeries+" 1\n", "", 1) != first {
		t.Fatalf("cached scrape differed beyond its request counter (reads: %d)", calls)
	}
	now = now.Add(2 * time.Second)
	down := metricsScrape(t, handler)
	if calls != 2 {
		t.Fatalf("expired cache made %d reads, want 2", calls)
	}
	samples = parseMetrics(t, down)
	assertMetric(t, samples, "hrn_metrics_database_up", 0)
	assertMetric(t, samples, "hrn_event_streams_open", 2)
	assertMetric(t, samples, scrapeSeries, 2)
	for _, name := range []string{"hrn_jobs", "hrn_schema_version", "hrn_tenants", "hrn_projects"} {
		if strings.Contains(down, name) {
			t.Errorf("database metric %s was exposed on error", name)
		}
	}
	assertMetric(t, parseMetrics(t, metricsScrape(t, handler)), "hrn_metrics_database_up", 1)
	if calls != 3 {
		t.Fatalf("error was cached: %d reads, want 3", calls)
	}
	if got := escapeMetricsLabel("plain\\\"\n"); got != `plain\\\"\n` {
		t.Fatalf("escaped label %q", got)
	}
}

type scrapeWaitContext struct {
	context.Context
	waiting chan<- struct{}
}

func (ctx scrapeWaitContext) Done() <-chan struct{} {
	ctx.waiting <- struct{}{}
	return ctx.Context.Done()
}

func TestMetricsReadOutlivesTheScrapeThatStartedIt(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var bounded atomic.Bool
	h := &HTTPServer{readMetrics: func(ctx context.Context) (StoreMetrics, error) {
		_, ok := ctx.Deadline()
		bounded.Store(ok)
		close(started)
		select {
		case <-release:
			return StoreMetrics{}, nil
		case <-ctx.Done():
			return StoreMetrics{}, ctx.Err()
		}
	}}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, err := h.databaseMetrics(ctx)
		first <- err
	}()
	<-started
	cancel()
	time.Sleep(50 * time.Millisecond)
	close(release)
	if err := <-first; err != nil {
		t.Errorf("the shared read ended with its first caller: %v", err)
	}
	if !bounded.Load() {
		t.Error("the shared read has no time limit")
	}
}

func TestMetricsConcurrentScrapesShareRead(t *testing.T) {
	for _, tc := range []struct {
		name, up string
		err      error
	}{{"success", "1", nil}, {"failure", "0", errors.New("database unavailable")}} {
		t.Run(tc.name, func(t *testing.T) {
			const followers = 4
			var reads atomic.Int32
			started, release := make(chan struct{}), make(chan struct{})
			waiting := make(chan struct{}, followers)
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			h := &HTTPServer{OperatorToken: operatorCredential, readMetrics: func(context.Context) (StoreMetrics, error) {
				if reads.Add(1) == 1 {
					close(started)
				}
				<-release
				return StoreMetrics{}, tc.err
			}}
			handler := h.Handler()
			var group sync.WaitGroup
			call := func(ctx context.Context) {
				r := httptest.NewRequestWithContext(ctx, "GET", "/metrics", nil)
				r.Header.Set("Authorization", "Bearer "+operatorCredential)
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if w.Code != 200 || !strings.Contains(w.Body.String(), "hrn_metrics_database_up "+tc.up+"\n") {
					t.Errorf("scrape status %d, body %s", w.Code, w.Body)
				}
			}
			group.Go(func() { call(context.Background()) })
			<-started
			for range followers {
				group.Go(func() { call(scrapeWaitContext{Context: context.Background(), waiting: waiting}) })
			}
			timeout := time.NewTimer(5 * time.Second)
			defer timeout.Stop()
			allWaiting := true
		wait:
			for range followers {
				select {
				case <-waiting:
				case <-timeout.C:
					allWaiting = false
					break wait
				}
			}
			unblock()
			group.Wait()
			if got := reads.Load(); !allWaiting || got != 1 {
				t.Fatalf("all followers joined the pending scrape: %t, reads: %d, want 1", allWaiting, got)
			}
		})
	}
}

func TestMetricsRequestAndSubmissionCounters(t *testing.T) {
	allow := func(context.Context, string, string) (bool, error) { return true, nil }
	h := &HTTPServer{
		OperatorToken: operatorCredential, MaxCostUSDPerJob: 0.5,
		submissionAuthorized: allow, tenantAuthorized: allow,
		submitJob: func(context.Context, Submission, bool) (Job, bool, error) {
			return Job{}, false, fmt.Errorf("%w: fixture budget", ErrTenantBudget)
		},
	}
	handler := h.Handler()
	for _, tc := range []struct {
		cost   float64
		reason string
	}{{1, "cost_limit"}, {0.5, "tenant_budget"}} {
		project := Project{ID: "private-project", TenantID: "private-tenant", BaseRef: fixtureRef, Backend: "api"}
		input := testSubmission(project, "", "inspect")
		input.Options.MaxCostUSD = tc.cost
		body, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		w := metricsRequest(handler, "POST", "/v1/projects/private-project/jobs", "fixture", string(body))
		if w.Code != 422 || !strings.Contains(w.Body.String(), `"code":"`+tc.reason+`"`) {
			t.Fatalf("submission status %d: %s", w.Code, w.Body)
		}
	}
	metricsRequest(handler, "POST", "/v1/tenants", "", "")
	metricsRequest(handler, "GET", "/private-path", "", "")
	metricsRequest(handler, "CUSTOM", "/v1/tenants", "", "")
	body := metricsScrape(t, handler)
	samples := parseMetrics(t, body)
	for name, want := range map[string]float64{
		`hrn_http_requests_total{route="POST /v1/projects/{project}/jobs",method="POST",code="422"}`: 2,
		`hrn_http_requests_total{route="POST /v1/tenants",method="POST",code="401"}`:                 1,
		`hrn_http_requests_total{route="other",method="GET",code="404"}`:                             1,
		`hrn_http_requests_total{route="other",method="other",code="405"}`:                           1,
		`hrn_submissions_refused_total{reason="cost_limit"}`:                                         1,
		`hrn_submissions_refused_total{reason="tenant_budget"}`:                                      1,
	} {
		assertMetric(t, samples, name, want)
	}
	if strings.Contains(body, "private-") || strings.Contains(body, "CUSTOM") {
		t.Fatal("request input leaked into metrics")
	}
}

func TestMetricsOIDCOutcomes(t *testing.T) {
	f := newOIDCFixture(t)
	claims := f.claims()
	claims["roles"] = "operator"
	signed := f.sign(t, oidcHeader("RS256", "rsa"), claims, "RS256")
	delete(claims, "roles")
	claims["tenants"] = []string{"own"}
	tenant := f.sign(t, oidcHeader("RS256", "rsa"), claims, "RS256")
	h := &HTTPServer{OIDC: f.verifier(t), OperatorToken: operatorCredential}
	handler := h.Handler()
	if w := metricsRequest(handler, "GET", "/metrics", signed, ""); w.Code != 200 {
		t.Fatalf("signed operator status %d: %s", w.Code, w.Body)
	}
	if w := metricsRequest(handler, "GET", "/metrics", tenant, ""); w.Code != 401 {
		t.Fatalf("signed tenant status %d: %s", w.Code, w.Body)
	}
	for _, tc := range []struct{ bearer, result string }{{"x.y.z", "refused"}, {signed, "unavailable"}} {
		if tc.result == "unavailable" {
			f.discovery.Store("invalid discovery")
			h.OIDC = f.verifier(t)
		}
		w := metricsRequest(handler, "GET", "/metrics", tc.bearer, "")
		if w.Code != 401 {
			t.Fatalf("%s token returned %d", tc.result, w.Code)
		}
	}
	samples := parseMetrics(t, metricsScrape(t, handler))
	for result, count := range map[string]float64{"accepted": 2, "refused": 1, "unavailable": 1} {
		assertMetric(t, samples, `hrn_oidc_tokens_total{result="`+result+`"}`, count)
	}
}

func TestMetricsRequestPatternAndDefaultStatus(t *testing.T) {
	h := &HTTPServer{OperatorToken: operatorCredential}
	mux := http.NewServeMux()
	mux.HandleFunc("CONNECT /{scope}/", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("GET /empty", func(http.ResponseWriter, *http.Request) {})
	handler := h.countRequests(mux)
	if w := metricsRequest(handler, "CONNECT", "/private-scope", "", ""); w.Code != 307 {
		t.Fatalf("CONNECT redirect status = %d, want 307", w.Code)
	}
	metricsRequest(handler, "GET", "/empty", "", "")
	body := metricsScrape(t, h.Handler())
	if strings.Contains(body, "private-scope") {
		t.Fatal("redirect path leaked into metrics")
	}
	samples := parseMetrics(t, body)
	assertMetric(t, samples, `hrn_http_requests_total{route="other",method="CONNECT",code="307"}`, 1)
	assertMetric(t, samples, `hrn_http_requests_total{route="GET /empty",method="GET",code="200"}`, 1)
}

type streamDeadlineRecorder struct {
	*httptest.ResponseRecorder
	deadline time.Time
}

func (w *streamDeadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}

func TestMetricsResponseWriterSupportsStreaming(t *testing.T) {
	h := &HTTPServer{OperatorToken: operatorCredential}
	deadline := time.Now()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stream", func(w http.ResponseWriter, _ *http.Request) {
		controller := http.NewResponseController(w)
		if err := controller.SetWriteDeadline(deadline); err != nil {
			t.Fatal(err)
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("stream writer lost http.Flusher")
		}
		flusher.Flush()
		if err := controller.Flush(); err != nil {
			t.Fatal(err)
		}
	})
	w := &streamDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	h.countRequests(mux).ServeHTTP(w, httptest.NewRequest("GET", "/stream", nil))
	if !w.Flushed || !w.deadline.Equal(deadline) || w.Code != 200 {
		t.Fatalf("stream writer flushed %t, deadline %v, status %d", w.Flushed, w.deadline, w.Code)
	}
	assertMetric(t, parseMetrics(t, metricsScrape(t, h.Handler())), `hrn_http_requests_total{route="GET /stream",method="GET",code="200"}`, 1)
}
