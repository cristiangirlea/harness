package watch

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/workmap"
)

func TestBotReportsWorkMapChanges(t *testing.T) {
	old := buildWorkMap
	t.Cleanup(func() { buildWorkMap = old })
	pieces := []workmap.Piece{{Repo: "/work/repo", Key: "one", Title: "sk-test-12345", Stage: workmap.Idea}}
	var buildErr error
	buildWorkMap = func(context.Context, string, []string) ([]workmap.Piece, error) {
		return append([]workmap.Piece(nil), pieces...), buildErr
	}
	r := httptest.NewRequest("GET", "http://localhost/", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	b := (&server{}).newBotWatcher(r)
	if got := b.lookMap(); len(got) != 0 {
		t.Fatalf("baseline: %+v", got)
	}
	pieces[0].Stuck = "gate failed"
	got := b.lookMap()
	if len(got) != 1 || got[0].Kind != "waits" || got[0].Ref != "one" || got[0].URL != "/map" || strings.Contains(got[0].Text, "sk-test") {
		t.Fatalf("stuck: %+v", got)
	}
	if got := b.lookMap(); len(got) != 0 {
		t.Fatalf("repeated: %+v", got)
	}
	pieces[0].Stage, pieces[0].Stuck = workmap.PR, ""
	got = b.lookMap()
	if len(got) != 1 || got[0].Kind != "map" || !strings.Contains(got[0].Text, "now at pr") {
		t.Fatalf("stage: %+v", got)
	}
	pieces = append(pieces, workmap.Piece{Repo: "/work/repo", Key: "two", Stage: workmap.Idea})
	if got := b.lookMap(); len(got) != 0 {
		t.Fatalf("new idea: %+v", got)
	}
	buildErr = errors.New("source")
	pieces[0].Stuck = "failed"
	if got := b.lookMap(); len(got) != 1 || got[0].Kind != "waits" {
		t.Fatalf("partial error: %+v", got)
	}
}

func TestBotDoesNotBuildMapForNonOwner(t *testing.T) {
	old := buildWorkMap
	t.Cleanup(func() { buildWorkMap = old })
	calls := 0
	buildWorkMap = func(context.Context, string, []string) ([]workmap.Piece, error) { calls++; return nil, nil }
	r := httptest.NewRequest("GET", "http://localhost/", nil)
	r.RemoteAddr = "192.0.2.1:1234"
	if got := (&server{}).newBotWatcher(r).lookMap(); len(got) != 0 || calls != 0 {
		t.Fatalf("messages %v builds %d", got, calls)
	}
	f := newAccessFixture(t)
	f.opt.OwnerEmails = []string{"owner@example.com"}
	s := &server{opt: f.opt}
	r = httptest.NewRequest("GET", "https://example.com/", nil)
	r = r.WithContext(context.WithValue(r.Context(), accessContextKey{}, &accessWriter{email: "other@example.com"}))
	if got := s.newBotWatcher(r).lookMap(); len(got) != 0 || calls != 0 {
		t.Fatalf("public messages %v builds %d", got, calls)
	}
}

func TestBotMapKindDefaults(t *testing.T) {
	for _, want := range []botKind{{"waits", "Something waits for you", true, true}, {"map", "Work moves on the map", false, false}} {
		found := false
		for _, kind := range botKinds {
			if kind == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing kind %+v", want)
		}
	}
	page, err := render("bot", botPage{Kinds: botKinds})
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{`data-kind="waits" data-how="speak" checked`, `data-kind="waits" data-how="notify" checked`, `data-kind="map" data-how="speak" aria-label=`, `data-kind="map" data-how="notify" aria-label=`} {
		if !strings.Contains(page, input) {
			t.Errorf("settings missing %q", input)
		}
	}
}

func TestBotPreservesHiddenPiecesDuringErrors(t *testing.T) {
	old := buildWorkMap
	t.Cleanup(func() { buildWorkMap = old })
	pieces := []workmap.Piece{{Repo: "r", Key: "x", Stage: workmap.Idea}, {Repo: "r", Key: "hidden", Stage: workmap.Idea}, {Repo: "r", Key: "y", Stage: workmap.Idea}}
	var buildErr error
	buildWorkMap = func(context.Context, string, []string) ([]workmap.Piece, error) { return pieces, buildErr }
	r := httptest.NewRequest("GET", "http://localhost/", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	b := (&server{}).newBotWatcher(r)
	if got := b.lookMap(); len(got) != 0 {
		t.Fatal(got)
	}
	pieces = []workmap.Piece{{Repo: "r", Key: "y", Stage: workmap.Idea, Stuck: "blocked"}}
	buildErr = errors.New("partial")
	if got := b.lookMap(); len(got) != 1 || got[0].Ref != "y" || got[0].Kind != "waits" {
		t.Fatalf("partial: %+v", got)
	}
	pieces = []workmap.Piece{{Repo: "r", Key: "x", Stage: workmap.Idea}, {Repo: "r", Key: "hidden", Stage: workmap.Idea, Stuck: "blocked"}, {Repo: "r", Key: "y", Stage: workmap.Idea, Stuck: "blocked"}}
	buildErr = nil
	if got := b.lookMap(); len(got) != 1 || got[0].Ref != "hidden" || got[0].Kind != "waits" {
		t.Fatalf("recovery: %+v", got)
	}
	if got := b.lookMap(); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestBotLinksNewPullRequests(t *testing.T) {
	old := buildWorkMap
	t.Cleanup(func() { buildWorkMap = old })
	var pieces []workmap.Piece
	buildWorkMap = func(context.Context, string, []string) ([]workmap.Piece, error) { return pieces, nil }
	r := httptest.NewRequest("GET", "http://localhost/", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	b := (&server{}).newBotWatcher(r)
	b.lookMap()
	pieces = []workmap.Piece{{Repo: "r", Key: "pr", Stage: workmap.PR, Links: workmap.Links{PR: "https://github.com/a/b/pull/1"}}}
	got := b.lookMap()
	if len(got) != 1 || got[0].Kind != "map" || got[0].URL != "https://github.com/a/b/pull/1" {
		t.Fatalf("PR: %+v", got)
	}
}

func TestPublicOwnerReceivesShortenedBotMessage(t *testing.T) {
	f := newAccessFixture(t)
	f.opt.OwnerEmails = []string{"owner@example.com"}
	old, interval := buildWorkMap, mapPoll
	t.Cleanup(func() { buildWorkMap = old; mapPoll = interval })
	mapPoll = 5 * time.Millisecond
	calls := 0
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	buildWorkMap = func(context.Context, string, []string) ([]workmap.Piece, error) {
		calls++
		p := workmap.Piece{Repo: "/work/repo", Key: "x", Title: "piece", Stage: workmap.Idea}
		if calls >= 2 {
			p.Stuck = "run /work/repo/gate"
			cancel()
		}
		return []workmap.Piece{p}, nil
	}
	r := httptest.NewRequest("GET", "https://harness.example.com/stream/bot", nil).WithContext(ctx)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Cf-Connecting-IP", "198.51.100.8")
	r.Header.Set("Cf-Access-Jwt-Assertion", f.validToken())
	w := httptest.NewRecorder()
	New(context.Background(), f.opt).ServeHTTP(w, r)
	body := w.Body.String()
	if calls < 2 || !strings.Contains(body, `"kind":"waits"`) || !strings.Contains(body, "./gate") || strings.Contains(body, "/work/repo") {
		t.Fatalf("calls %d: %s", calls, body)
	}
}

func TestBotStreamPollsMapIndependentlyAndOnlyForOwner(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	old, interval := buildWorkMap, mapPoll
	t.Cleanup(func() { buildWorkMap = old; mapPoll = interval })
	mapPoll = 80 * time.Millisecond
	for _, owner := range []bool{true, false} {
		calls := 0
		ctx, cancel := context.WithTimeout(context.Background(), 230*time.Millisecond)
		buildWorkMap = func(context.Context, string, []string) ([]workmap.Piece, error) {
			calls++
			return []workmap.Piece{{Repo: "r", Key: "x", Stage: workmap.Idea, Stuck: "existing"}}, nil
		}
		r := httptest.NewRequest("GET", "http://localhost/stream/bot", nil).WithContext(ctx)
		r.RemoteAddr = "192.0.2.1:1234"
		if owner {
			r.RemoteAddr = "127.0.0.1:1234"
		}
		w := httptest.NewRecorder()
		New(context.Background(), Options{Poll: 2 * time.Millisecond}).ServeHTTP(w, r)
		cancel()
		if owner && (calls < 2 || calls > 4 || strings.Contains(w.Body.String(), `"kind":"waits"`)) {
			t.Fatalf("owner builds %d: %s", calls, w.Body)
		}
		if !owner && calls != 0 {
			t.Fatalf("non-owner builds %d", calls)
		}
	}
}

func TestBotAnnouncesOnlyGatesOrLater(t *testing.T) {
	old := buildWorkMap
	t.Cleanup(func() { buildWorkMap = old })
	pieces := []workmap.Piece{{Repo: "repo", Key: "feature", Title: "feature", Stage: workmap.Agent}}
	buildWorkMap = func(context.Context, string, []string) ([]workmap.Piece, error) { return pieces, nil }
	r := httptest.NewRequest("GET", "http://localhost/", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	b := (&server{}).newBotWatcher(r)
	b.lookMap()
	for _, stage := range []workmap.Stage{workmap.Branch, workmap.Agent} {
		pieces[0].Stage = stage
		if got := b.lookMap(); len(got) != 0 {
			t.Fatalf("%s: %+v", stage, got)
		}
	}
	pieces = nil
	b.lookMap()
	pieces = []workmap.Piece{{Repo: "repo", Key: "feature", Stage: workmap.Agent}}
	if got := b.lookMap(); len(got) != 0 {
		t.Fatalf("return: %+v", got)
	}
	for _, stage := range []workmap.Stage{workmap.Gates, workmap.PR} {
		pieces[0].Stage = stage
		if got := b.lookMap(); len(got) != 1 || got[0].Kind != "map" {
			t.Fatalf("%s: %+v", stage, got)
		}
	}
}
