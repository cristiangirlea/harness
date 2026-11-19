package platform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type requestSeries struct {
	route, method string
	code          int
}

type processCounters struct {
	mu              sync.Mutex
	requests        map[requestSeries]uint64
	costLimit       atomic.Uint64
	tenantBudget    atomic.Uint64
	oidcAccepted    atomic.Uint64
	oidcRefused     atomic.Uint64
	oidcUnavailable atomic.Uint64
}

func (h *HTTPServer) countRequests(mux http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorded := &metricsResponseWriter{ResponseWriter: w}
		mux.ServeHTTP(recorded, r)
		route := r.Pattern
		// CONNECT redirects can report a request path as the pattern.
		if route == "" || (r.Method == http.MethodConnect && recorded.code == http.StatusTemporaryRedirect && strings.HasPrefix(route, "/")) {
			route = "other"
		}
		code := recorded.code
		if code == 0 {
			code = http.StatusOK
		}
		key := requestSeries{route, metricsMethod(r.Method), code}
		h.counters.mu.Lock()
		if h.counters.requests == nil {
			h.counters.requests = make(map[requestSeries]uint64)
		}
		h.counters.requests[key]++
		h.counters.mu.Unlock()
	})
}

func metricsMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace:
		return method
	default:
		return "other"
	}
}

type metricsResponseWriter struct {
	http.ResponseWriter
	code int
}

func (w *metricsResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *metricsResponseWriter) WriteHeader(code int) {
	w.ResponseWriter.WriteHeader(code)
	if w.code == 0 && (code == http.StatusSwitchingProtocols || code >= 200) {
		w.code = code
	}
}

func (w *metricsResponseWriter) Write(p []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *metricsResponseWriter) Flush() { _ = w.FlushError() }

func (w *metricsResponseWriter) FlushError() error {
	if err := http.NewResponseController(w.ResponseWriter).Flush(); err != nil {
		return err
	}
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return nil
}

func (h *HTTPServer) submissionRefused(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrCostLimit):
		h.counters.costLimit.Add(1)
	case errors.Is(err, ErrTenantBudget):
		h.counters.tenantBudget.Add(1)
	}
	invalid(w, err)
}

func (s *eventStreams) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, active := range s.active {
		total += active
	}
	return total
}

type metricsRead struct {
	done  chan struct{}
	value StoreMetrics
	err   error
}

type metricsCache struct {
	mu        sync.Mutex
	pending   *metricsRead
	value     StoreMetrics
	expiresAt time.Time
}

func (h *HTTPServer) databaseMetrics(ctx context.Context) (StoreMetrics, error) {
	now := h.metricsNow
	if now == nil {
		now = time.Now
	}
	cache := &h.metricsCache
	cache.mu.Lock()
	if !cache.expiresAt.IsZero() && now().Before(cache.expiresAt) {
		value := cache.value
		cache.mu.Unlock()
		return value, nil
	}
	if read := cache.pending; read != nil {
		cache.mu.Unlock()
		select {
		case <-read.done:
			return read.value, read.err
		case <-ctx.Done():
			return StoreMetrics{}, ctx.Err()
		}
	}
	read := &metricsRead{done: make(chan struct{})}
	cache.pending = read
	cache.mu.Unlock()

	// Other scrapes share this read, so the one that started it must not be able to cancel it.
	shared, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	switch {
	case h.readMetrics != nil:
		read.value, read.err = h.readMetrics(shared)
	case h.Store != nil && h.Store.db != nil:
		read.value, read.err = h.Store.Metrics(shared)
	default:
		read.err = errors.New("metrics storage unavailable")
	}

	cache.mu.Lock()
	if read.err == nil {
		cache.value = read.value
		cache.expiresAt = now().Add(5 * time.Second)
	}
	cache.pending = nil
	close(read.done)
	cache.mu.Unlock()
	return read.value, read.err
}

var metricsLabelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escapeMetricsLabel(value string) string { return metricsLabelEscaper.Replace(value) }

var metricsBuildVersion = func() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "unknown"
}()

func metricFamily(w io.Writer, name, kind, help string) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}

func (h *HTTPServer) metrics(w http.ResponseWriter, r *http.Request) {
	if !sameToken(token(r), h.MetricsToken) && !h.operator(w, r) {
		return
	}
	database, err := h.databaseMetrics(r.Context())
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	metricFamily(w, "hrn_build_info", "gauge", "Control service build version.")
	fmt.Fprintf(w, "hrn_build_info{version=\"%s\"} 1\n", escapeMetricsLabel(metricsBuildVersion))
	metricFamily(w, "hrn_event_streams_open", "gauge", "Currently open event streams.")
	fmt.Fprintf(w, "hrn_event_streams_open %d\n", h.eventStreams.count())
	h.counters.writeRequests(w)
	if err == nil {
		metricFamily(w, "hrn_jobs", "gauge", "Jobs by state.")
		for _, state := range metricJobStates {
			fmt.Fprintf(w, "hrn_jobs{state=\"%s\"} %d\n", state, database.Jobs[state])
		}
		metricFamily(w, "hrn_jobs_oldest_queued_seconds", "gauge", "Age of the oldest queued job in seconds.")
		fmt.Fprintf(w, "hrn_jobs_oldest_queued_seconds %g\n", database.OldestQueuedSeconds)
	}
	metricFamily(w, "hrn_metrics_database_up", "gauge", "Whether database metrics are available.")
	up := 0
	if err == nil {
		up = 1
	}
	fmt.Fprintf(w, "hrn_metrics_database_up %d\n", up)
	metricFamily(w, "hrn_oidc_tokens_total", "counter", "Signed token verifications by result.")
	fmt.Fprintf(w, "hrn_oidc_tokens_total{result=\"accepted\"} %d\n", h.counters.oidcAccepted.Load())
	fmt.Fprintf(w, "hrn_oidc_tokens_total{result=\"refused\"} %d\n", h.counters.oidcRefused.Load())
	fmt.Fprintf(w, "hrn_oidc_tokens_total{result=\"unavailable\"} %d\n", h.counters.oidcUnavailable.Load())
	if err == nil {
		metricFamily(w, "hrn_projects", "gauge", "Registered projects.")
		fmt.Fprintf(w, "hrn_projects %d\n", database.Projects)
		metricFamily(w, "hrn_schema_version", "gauge", "Latest applied database schema version.")
		fmt.Fprintf(w, "hrn_schema_version %d\n", database.SchemaVersion)
	}
	metricFamily(w, "hrn_submissions_refused_total", "counter", "Job submissions refused by reason.")
	fmt.Fprintf(w, "hrn_submissions_refused_total{reason=\"cost_limit\"} %d\n", h.counters.costLimit.Load())
	fmt.Fprintf(w, "hrn_submissions_refused_total{reason=\"tenant_budget\"} %d\n", h.counters.tenantBudget.Load())
	if err == nil {
		metricFamily(w, "hrn_tenants", "gauge", "Registered tenants.")
		fmt.Fprintf(w, "hrn_tenants %d\n", database.Tenants)
	}
}

func (c *processCounters) writeRequests(w io.Writer) {
	c.mu.Lock()
	lines := make([]string, 0, len(c.requests))
	for key, count := range c.requests {
		lines = append(lines, fmt.Sprintf("hrn_http_requests_total{route=\"%s\",method=\"%s\",code=\"%d\"} %d\n",
			escapeMetricsLabel(key.route), key.method, key.code, count))
	}
	c.mu.Unlock()
	slices.Sort(lines)
	metricFamily(w, "hrn_http_requests_total", "counter", "Completed HTTP requests by route, method, and status.")
	for _, line := range lines {
		io.WriteString(w, line)
	}
}
