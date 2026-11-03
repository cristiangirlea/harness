package watch

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/cristiangirlea/harness/internal/workmap"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	old := buildWorkMap
	t.Cleanup(func() { buildWorkMap = old })
	buildWorkMap = func(context.Context, string, []string) ([]workmap.Piece, error) {
		return []workmap.Piece{{Repo: "repo", Title: "local piece", Stage: workmap.Idea}}, errors.New("failed")
	}
	h := New(context.Background(), Options{})
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
	public := New(context.Background(), f.opt)
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
	old, interval := buildWorkMap, mapPoll
	t.Cleanup(func() { buildWorkMap = old; mapPoll = interval })
	mapPoll = 5 * time.Millisecond
	calls := 0
	buildWorkMap = func(context.Context, string, []string) ([]workmap.Piece, error) {
		calls++
		title := "first"
		if calls >= 3 {
			title = "second"
		}
		return []workmap.Piece{{Repo: "r", Title: title, Stage: workmap.Idea}}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest("GET", "http://localhost/stream/map", nil).WithContext(ctx)
	r.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	New(context.Background(), Options{}).ServeHTTP(w, r)
	body := w.Body.String()
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
	old := buildWorkMap
	t.Cleanup(func() { buildWorkMap = old })
	buildWorkMap = func(context.Context, string, []string) ([]workmap.Piece, error) { return pieces, nil }
	w := httptest.NewRecorder()
	New(context.Background(), Options{}).ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `class="map-svg"`) {
		t.Error("missing scroll container")
	}
}
