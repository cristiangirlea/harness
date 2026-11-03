package watch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/workmap"
)

func TestWorkMapEscapesTextAndSelectsSafeLinks(t *testing.T) {
	p := []workmap.Piece{{Repo: `repo<&"`, Title: `title<&"`, Stage: workmap.Idea, Stuck: `wait<&"`, Links: workmap.Links{PR: "javascript:alert(1)", Runs: []string{"20260101-000000-aabbcc"}}}, {Repo: "other", Title: "pull", Stage: workmap.CI, Links: workmap.Links{PR: "https://github.com/a/b/pull/1"}}, {Repo: "other", Title: "plain", Stage: workmap.Branch, Links: workmap.Links{Issue: "http://bad"}}}
	v := mapViewOf(p, nil)
	for _, want := range []string{"&lt;", "&amp;", "&#34;", "map-stuck", "/flow/20260101-000000-aabbcc", "https://github.com/a/b/pull/1", "released"} {
		if !strings.Contains(v.SVG, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, bad := range []string{"javascript:", "http://bad", "style=", "<script"} {
		if strings.Contains(v.SVG, bad) {
			t.Errorf("unsafe %q", bad)
		}
	}
	if len(v.Stuck) != 1 {
		t.Fatalf("stuck: %+v", v.Stuck)
	}
}

func TestWorkMapRestrictsAccessAndShowsPartialErrors(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	maps := defaultMapSettings()
	maps.build = func(context.Context, string, []string) ([]workmap.Piece, error) {
		return []workmap.Piece{{Repo: "repo", Title: "local piece", Stage: workmap.Idea}}, errors.New("failed")
	}
	h := newHandlerWithMap(context.Background(), Options{}, nil, maps)
	for _, path := range []string{"/map", "/stream/map"} {
		r := httptest.NewRequest("GET", "http://localhost"+path, nil)
		r.RemoteAddr = "192.168.1.5:1234"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	r := httptest.NewRequest("GET", "http://localhost/map", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("Content-Security-Policy") != csp || !strings.Contains(w.Body.String(), "local piece") || !strings.Contains(w.Body.String(), "could not be read") {
		t.Fatalf("page: %d %s", w.Code, w.Body)
	}
	f := newAccessFixture(t)
	f.opt.OwnerEmails = []string{"owner@example.com"}
	public := newHandlerWithMap(context.Background(), f.opt, nil, maps)
	for _, path := range []string{"/map", "/stream/map"} {
		if got := f.request(public, path, "harness.example.com", "", f.tokenFor("other@example.com")); got.Code != 404 {
			t.Fatalf("public %s: %d", path, got.Code)
		}
	}
	if got := f.request(public, "/map", "harness.example.com", "", f.validToken()); got.Code != 200 {
		t.Fatalf("owner: %d", got.Code)
	}
}
func TestWorkMapStreamSendsOnlyChangedSnapshots(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	maps := defaultMapSettings()
	maps.poll = 5 * time.Millisecond
	var calls atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	maps.build = func(context.Context, string, []string) ([]workmap.Piece, error) {
		n := calls.Add(1)
		title := "first"
		if n >= 3 {
			title = "second"
		}
		return []workmap.Piece{{Repo: "r", Title: title, Stage: workmap.Idea}}, nil
	}
	r := httptest.NewRequest("GET", "http://localhost/stream/map", nil).WithContext(ctx)
	r.RemoteAddr = "127.0.0.1:1234"
	w := &cancelMapWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel, events: 2}
	newHandlerWithMap(context.Background(), Options{}, nil, maps).ServeHTTP(w, r)
	body := w.Body.String()
	if ctx.Err() != context.Canceled || calls.Load() < 3 {
		t.Fatalf("stream did not stop on cancellation: %v, %d calls", ctx.Err(), calls.Load())
	}
	if strings.Count(body, "event: replace") != 2 {
		t.Fatalf("events: %s", body)
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: {") {
			var v mapView
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v); err != nil || v.SVG == "" || v.Stuck == nil {
				t.Fatalf("JSON: %s %v", line, err)
			}
		}
	}
}

func TestWorkMapUsesShortRepositoryNamesAndScrollableWidth(t *testing.T) {
	pieces := []workmap.Piece{
		{Repo: `E:\very\long\parent\windows-repo`, Title: "one", Stage: workmap.Idea},
		{Repo: "/very/long/parent/posix-repo", Title: "two", Stage: workmap.Idea},
		{Repo: `/first/shared`, Title: "three", Stage: workmap.Idea},
		{Repo: `C:\second\shared`, Title: "four", Stage: workmap.Idea},
	}
	v := mapViewOf(pieces, nil)
	for _, want := range []string{"windows-repo", "posix-repo", "first/shared", "second/shared", `width="1260"`} {
		if !strings.Contains(v.SVG, want) {
			t.Errorf("missing %q in %s", want, v.SVG)
		}
	}
	for _, bad := range []string{`:\`, "/very/long", "/first/shared"} {
		if strings.Contains(v.SVG, bad) {
			t.Errorf("found path %q in SVG", bad)
		}
	}
	r := httptest.NewRequest("GET", "http://localhost/map", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	maps := defaultMapSettings()
	maps.build = func(context.Context, string, []string) ([]workmap.Piece, error) { return pieces, nil }
	w := httptest.NewRecorder()
	newHandlerWithMap(context.Background(), Options{}, nil, maps).ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `class="map-svg"`) {
		t.Error("missing scroll container")
	}
}

func TestWorkMapMasksSecretsAndShortensRemotePaths(t *testing.T) {
	maps := defaultMapSettings()
	maps.build = func(context.Context, string, []string) ([]workmap.Piece, error) {
		return []workmap.Piece{{Repo: "/work/repo", Title: "sk-test-12345", Stuck: "run /work/repo/gate sk-test-12345", Stage: workmap.Idea}}, nil
	}
	s := &server{mapConfig: maps}
	local := httptest.NewRequest("GET", "http://localhost/map", nil)
	local.RemoteAddr = "127.0.0.1:1234"
	remote := httptest.NewRequest("GET", "http://localhost/map", nil)
	remote.RemoteAddr = "192.168.1.5:1234"
	for _, tc := range []struct {
		r    *http.Request
		path string
	}{{local, "/work/repo/gate"}, {remote, "./gate"}} {
		v := s.mapData(tc.r.Context(), s.viewerOf(tc.r))
		if strings.Contains(v.SVG, "sk-test-12345") || strings.Contains(v.Stuck[0].Title, "sk-test-12345") || strings.Contains(v.Stuck[0].Stuck, "sk-test-12345") || !strings.Contains(v.Stuck[0].Stuck, tc.path) {
			t.Fatalf("view: %+v", v)
		}
		if !strings.Contains(v.SVG, "<title>\u2022\u2022\u2022\u2022</title>") || !strings.Contains(v.SVG, "\u2022\u2022\u2022\u2022</text>") {
			t.Fatalf("SVG: %s", v.SVG)
		}
	}
}

func TestWorkMapEscapesFallbackAndRejectsUnsafeStuckLinks(t *testing.T) {
	v := mapViewOf([]workmap.Piece{{Repo: "r", Stage: workmap.Idea, Links: workmap.Links{Branch: `<&"`, PR: "javascript:alert(1)"}, Stuck: "blocked"}, {Repo: "r", Stage: workmap.Idea, Stuck: "blocked", Links: workmap.Links{Issue: "http://bad"}}}, nil)
	if !strings.Contains(v.SVG, "&lt;&amp;&#34;") {
		t.Fatal(v.SVG)
	}
	for _, item := range v.Stuck {
		if item.URL != "" {
			t.Fatalf("unsafe URL: %q", item.URL)
		}
	}
}

func TestWorkMapLinkFollowsOwnerAccess(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	h := New(context.Background(), Options{})
	for _, tc := range []struct {
		addr string
		want bool
	}{{"127.0.0.1:1234", true}, {"192.168.1.5:1234", false}} {
		r := httptest.NewRequest("GET", "http://localhost/", nil)
		r.RemoteAddr = tc.addr
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if strings.Contains(w.Body.String(), `href="/map"`) != tc.want {
			t.Fatalf("%s: %s", tc.addr, w.Body)
		}
	}
	f := newAccessFixture(t)
	f.opt.OwnerEmails = []string{"owner@example.com"}
	h = New(context.Background(), f.opt)
	for _, tc := range []struct {
		token string
		want  bool
	}{{f.validToken(), true}, {f.tokenFor("other@example.com"), false}} {
		w := f.request(h, "/", "harness.example.com", "", tc.token)
		if strings.Contains(w.Body.String(), `href="/map"`) != tc.want {
			t.Fatalf("public index: %s", w.Body)
		}
	}
}

func TestPublicOwnerReceivesMaskedWorkMapStream(t *testing.T) {
	f := newAccessFixture(t)
	f.opt.OwnerEmails = []string{"owner@example.com"}
	maps := defaultMapSettings()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	maps.build = func(context.Context, string, []string) ([]workmap.Piece, error) {
		return []workmap.Piece{{Repo: "/work/repo", Title: "sk-test-12345", Stuck: "run /work/repo/gate sk-test-12345\nsecond line", Stage: workmap.Idea}}, nil
	}
	r := httptest.NewRequest("GET", "https://harness.example.com/stream/map", nil).WithContext(ctx)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Cf-Access-Jwt-Assertion", f.validToken())
	w := &cancelMapWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel, events: 1}
	newHandlerWithMap(context.Background(), f.opt, nil, maps).ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "event: replace") {
		t.Fatalf("stream: %d %s", w.Code, w.Body)
	}
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if strings.HasPrefix(line, "data: {") {
			var v mapView
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v); err != nil {
				t.Fatal(err)
			}
			if len(v.Stuck) != 1 || v.Stuck[0].Title != "••••" || strings.Contains(v.Stuck[0].Stuck, "sk-test") || !strings.Contains(v.Stuck[0].Stuck, "./gate") || strings.Contains(v.Stuck[0].Stuck, "/work/repo") || strings.Contains(v.Stuck[0].Stuck, "second line") || strings.Contains(v.SVG, "sk-test-12345") {
				t.Fatalf("stream view: %+v", v)
			}
			return
		}
	}
	t.Fatal("missing JSON")
}

type cancelMapWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
	events int
}

func (w *cancelMapWriter) Flush() {
	w.ResponseRecorder.Flush()
	if strings.Count(w.Body.String(), "event: replace") >= w.events && strings.Count(w.Body.String(), "data: {") >= w.events {
		w.cancel()
	}
}

func TestWorkMapMasksASecretBeforeCuttingItsText(t *testing.T) {
	maps := defaultMapSettings()
	maps.build = func(context.Context, string, []string) ([]workmap.Piece, error) {
		long := "deploy the release with sk-test-1234567890abcdef"
		return []workmap.Piece{{Repo: "r", Title: long, Stuck: long, Stage: workmap.Idea}}, nil
	}
	r := httptest.NewRequest("GET", "http://localhost/map", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	s := &server{mapConfig: maps}
	v := s.mapData(r.Context(), s.viewerOf(r))
	if strings.Contains(v.SVG, "sk-") || strings.Contains(v.Stuck[0].Title, "sk-") || strings.Contains(v.Stuck[0].Stuck, "sk-") {
		t.Fatalf("a fragment of the secret survived the cut: %+v", v)
	}
}

func TestMapShowsActiveSessionWithoutItsID(t *testing.T) {
	v := mapViewOf([]workmap.Piece{{Repo: "repo", Title: "feature", Stage: workmap.Agent, ActiveSession: true, Links: workmap.Links{Sessions: []string{"private-session-id"}}}}, nil)
	if !strings.Contains(v.SVG, "map-session") || !strings.Contains(v.SVG, "Claude Code session") || strings.Contains(v.SVG, "private-session-id") {
		t.Fatalf("SVG: %s", v.SVG)
	}
}
