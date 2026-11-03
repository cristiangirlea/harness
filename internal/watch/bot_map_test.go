package watch

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/workmap"
)

func TestBotReportsWorkMapChanges(t *testing.T) {
	maps := defaultMapSettings()
	pieces := []workmap.Piece{{Repo: "/work/repo", Key: "one", Title: "sk-test-12345", Stage: workmap.Idea}}
	var buildErr error
	maps.build = func(context.Context, string, []string) ([]workmap.Piece, error) {
		return append([]workmap.Piece(nil), pieces...), buildErr
	}
	r := httptest.NewRequest("GET", "http://localhost/", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	b := (&server{mapConfig: maps}).newBotWatcher(r)
	if got := freshBotMap(b); len(got) != 0 {
		t.Fatalf("baseline: %+v", got)
	}
	pieces[0].Stuck = "gate failed"
	got := freshBotMap(b)
	if len(got) != 1 || got[0].Kind != "waits" || got[0].Ref != "one" || got[0].URL != "/map" || strings.Contains(got[0].Text, "sk-test") {
		t.Fatalf("stuck: %+v", got)
	}
	if got := freshBotMap(b); len(got) != 0 {
		t.Fatalf("repeated: %+v", got)
	}
	pieces[0].Stage, pieces[0].Stuck = workmap.PR, ""
	got = freshBotMap(b)
	if len(got) != 1 || got[0].Kind != "map" || !strings.Contains(got[0].Text, "now at pr") {
		t.Fatalf("stage: %+v", got)
	}
	pieces = append(pieces, workmap.Piece{Repo: "/work/repo", Key: "two", Stage: workmap.Idea})
	if got := freshBotMap(b); len(got) != 0 {
		t.Fatalf("new idea: %+v", got)
	}
	buildErr = errors.New("source")
	pieces[0].Stuck = "failed"
	if got := freshBotMap(b); len(got) != 1 || got[0].Kind != "waits" {
		t.Fatalf("partial error: %+v", got)
	}
}

func TestBotDoesNotBuildMapForNonOwner(t *testing.T) {
	maps := defaultMapSettings()
	calls := 0
	maps.build = func(context.Context, string, []string) ([]workmap.Piece, error) { calls++; return nil, nil }
	r := httptest.NewRequest("GET", "http://localhost/", nil)
	r.RemoteAddr = "192.0.2.1:1234"
	if got := (&server{mapConfig: maps}).newBotWatcher(r).lookMap(); len(got) != 0 || calls != 0 {
		t.Fatalf("messages %v builds %d", got, calls)
	}
	f := newAccessFixture(t)
	f.opt.OwnerEmails = []string{"owner@example.com"}
	s := &server{opt: f.opt, mapConfig: maps}
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
	maps := defaultMapSettings()
	pieces := []workmap.Piece{{Repo: "r", Key: "x", Stage: workmap.Idea}, {Repo: "r", Key: "hidden", Stage: workmap.Idea}, {Repo: "r", Key: "y", Stage: workmap.Idea}}
	var buildErr error
	maps.build = func(context.Context, string, []string) ([]workmap.Piece, error) { return pieces, buildErr }
	r := httptest.NewRequest("GET", "http://localhost/", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	b := (&server{mapConfig: maps}).newBotWatcher(r)
	if got := freshBotMap(b); len(got) != 0 {
		t.Fatal(got)
	}
	pieces = []workmap.Piece{{Repo: "r", Key: "y", Stage: workmap.Idea, Stuck: "blocked"}}
	buildErr = errors.New("partial")
	if got := freshBotMap(b); len(got) != 1 || got[0].Ref != "y" || got[0].Kind != "waits" {
		t.Fatalf("partial: %+v", got)
	}
	pieces = []workmap.Piece{{Repo: "r", Key: "x", Stage: workmap.Idea}, {Repo: "r", Key: "hidden", Stage: workmap.Idea, Stuck: "blocked"}, {Repo: "r", Key: "y", Stage: workmap.Idea, Stuck: "blocked"}}
	buildErr = nil
	if got := freshBotMap(b); len(got) != 1 || got[0].Ref != "hidden" || got[0].Kind != "waits" {
		t.Fatalf("recovery: %+v", got)
	}
	if got := freshBotMap(b); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestBotLinksNewPullRequests(t *testing.T) {
	maps := defaultMapSettings()
	var pieces []workmap.Piece
	maps.build = func(context.Context, string, []string) ([]workmap.Piece, error) { return pieces, nil }
	r := httptest.NewRequest("GET", "http://localhost/", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	b := (&server{mapConfig: maps}).newBotWatcher(r)
	freshBotMap(b)
	pieces = []workmap.Piece{{Repo: "r", Key: "pr", Stage: workmap.PR, Links: workmap.Links{PR: "https://github.com/a/b/pull/1"}}}
	got := freshBotMap(b)
	if len(got) != 1 || got[0].Kind != "map" || got[0].URL != "https://github.com/a/b/pull/1" {
		t.Fatalf("PR: %+v", got)
	}
}

func TestPublicOwnerReceivesShortenedBotMessage(t *testing.T) {
	f := newAccessFixture(t)
	f.opt.OwnerEmails = []string{"owner@example.com"}
	maps := defaultMapSettings()
	maps.poll = 5 * time.Millisecond
	var calls atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	maps.build = func(context.Context, string, []string) ([]workmap.Piece, error) {
		n := calls.Add(1)
		p := workmap.Piece{Repo: "/work/repo", Key: "x", Title: "piece", Stage: workmap.Idea}
		if n >= 2 {
			p.Stuck = "run /work/repo/gate"
			go func() { time.Sleep(50 * time.Millisecond); cancel() }()
		}
		return []workmap.Piece{p}, nil
	}
	r := httptest.NewRequest("GET", "https://harness.example.com/stream/bot", nil).WithContext(ctx)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Cf-Connecting-IP", "198.51.100.8")
	r.Header.Set("Cf-Access-Jwt-Assertion", f.validToken())
	w := httptest.NewRecorder()
	newHandlerWithMap(ctx, f.opt, nil, maps).ServeHTTP(w, r)
	body := w.Body.String()
	if calls.Load() < 2 || !strings.Contains(body, `"kind":"waits"`) || !strings.Contains(body, "./gate") || strings.Contains(body, "/work/repo") {
		t.Fatalf("calls %d: %s", calls.Load(), body)
	}
}

func TestBotStreamPollsMapIndependentlyAndOnlyForOwner(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	maps := defaultMapSettings()
	maps.poll = 80 * time.Millisecond
	for _, owner := range []bool{true, false} {
		var calls atomic.Int32
		ctx, cancel := context.WithTimeout(context.Background(), 230*time.Millisecond)
		maps.build = func(context.Context, string, []string) ([]workmap.Piece, error) {
			calls.Add(1)
			return []workmap.Piece{{Repo: "r", Key: "x", Stage: workmap.Idea, Stuck: "existing"}}, nil
		}
		r := httptest.NewRequest("GET", "http://localhost/stream/bot", nil).WithContext(ctx)
		r.RemoteAddr = "192.0.2.1:1234"
		if owner {
			r.RemoteAddr = "127.0.0.1:1234"
		}
		w := httptest.NewRecorder()
		newHandlerWithMap(ctx, Options{Poll: 2 * time.Millisecond}, nil, maps).ServeHTTP(w, r)
		cancel()
		if owner && (calls.Load() < 2 || calls.Load() > 4 || !strings.Contains(w.Body.String(), `"kind":"waits"`)) {
			t.Fatalf("owner builds %d: %s", calls.Load(), w.Body)
		}
		if !owner && calls.Load() != 0 {
			t.Fatalf("non-owner builds %d", calls.Load())
		}
	}
}

func TestBotAnnouncesOnlyGatesOrLater(t *testing.T) {
	maps := defaultMapSettings()
	pieces := []workmap.Piece{{Repo: "repo", Key: "feature", Title: "feature", Stage: workmap.Agent}}
	maps.build = func(context.Context, string, []string) ([]workmap.Piece, error) { return pieces, nil }
	r := httptest.NewRequest("GET", "http://localhost/", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	b := (&server{mapConfig: maps}).newBotWatcher(r)
	freshBotMap(b)
	for _, stage := range []workmap.Stage{workmap.Branch, workmap.Agent} {
		pieces[0].Stage = stage
		if got := freshBotMap(b); len(got) != 0 {
			t.Fatalf("%s: %+v", stage, got)
		}
	}
	pieces = nil
	freshBotMap(b)
	pieces = []workmap.Piece{{Repo: "repo", Key: "feature", Stage: workmap.Agent}}
	if got := freshBotMap(b); len(got) != 0 {
		t.Fatalf("return: %+v", got)
	}
	for _, stage := range []workmap.Stage{workmap.Gates, workmap.PR} {
		pieces[0].Stage = stage
		if got := freshBotMap(b); len(got) != 1 || got[0].Kind != "map" {
			t.Fatalf("%s: %+v", stage, got)
		}
	}
}

func freshBotMap(b *botWatcher) []say {
	b.s.mapCache.mu.Lock()
	b.s.mapCache.at = time.Time{}
	b.s.mapCache.mu.Unlock()
	return b.lookMap()
}

func TestBotKeepsPullRequestIdentity(t *testing.T) {
	maps := defaultMapSettings()
	piece := workmap.Piece{Repo: "r", Key: "pr#1", Stage: workmap.PR, Links: workmap.Links{PR: "https://github.com/a/b/pull/1"}}
	maps.build = func(context.Context, string, []string) ([]workmap.Piece, error) { return []workmap.Piece{piece}, nil }
	r := httptest.NewRequest("GET", "http://localhost/", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	b := (&server{mapConfig: maps}).newBotWatcher(r)
	freshBotMap(b)
	piece.Key = "branch:feature"
	if got := freshBotMap(b); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestFirstMapLookSummarizesWaitingPieces(t *testing.T) {
	maps := defaultMapSettings()
	maps.build = func(context.Context, string, []string) ([]workmap.Piece, error) {
		return []workmap.Piece{{Title: "alpha", Stuck: "blocked"}, {Title: "beta", Stuck: "blocked"}}, nil
	}
	r := httptest.NewRequest("GET", "http://localhost/", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	got := (&server{mapConfig: maps}).newBotWatcher(r).lookMap()
	if len(got) != 1 || got[0].Kind != "waits" || got[0].Text != "two pieces wait for you: alpha and beta." {
		t.Fatalf("summary: %+v", got)
	}
}

func TestBotPageMapOwner(t *testing.T) {
	maps := defaultMapSettings()
	maps.build = func(context.Context, string, []string) ([]workmap.Piece, error) {
		return []workmap.Piece{{Title: "one", Stuck: "blocked"}, {Title: "two", Stuck: "blocked"}}, nil
	}
	for _, owner := range []bool{true, false} {
		r := httptest.NewRequest("GET", "http://localhost/bot", nil)
		r.RemoteAddr = "192.0.2.1:1234"
		if owner {
			r.RemoteAddr = "127.0.0.1:1234"
		}
		w := httptest.NewRecorder()
		s := &server{mapConfig: maps}
		if owner {
			s.sharedMap(context.Background())
		}
		s.bot(w, r)
		body := w.Body.String()
		if strings.Contains(body, `href="/map"`) != owner || strings.Contains(body, "Two pieces wait for you.") != owner {
			t.Fatalf("owner %v: %s", owner, body)
		}
	}
}

func TestBotPageDoesNotWaitForMap(t *testing.T) {
	maps := defaultMapSettings()
	started := make(chan struct{})
	stopped := make(chan struct{})
	maps.build = func(ctx context.Context, _ string, _ []string) ([]workmap.Piece, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &server{ctx: ctx, mapConfig: maps}
	r := httptest.NewRequest("GET", "http://localhost/bot", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	done := make(chan struct{})
	go func() { s.bot(httptest.NewRecorder(), r); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		cancel()
		<-stopped
		t.Fatal("bot page waited for map")
	}
	<-started
	cancel()
	<-stopped
}

func TestFirstMapLookWaitingCounts(t *testing.T) {
	maps := defaultMapSettings()
	for _, tc := range []struct {
		names []string
		want  string
	}{
		{[]string{"alpha"}, "one piece waits for you: alpha."},
		{[]string{"alpha", "beta", "gamma", "delta", "epsilon"}, "five pieces wait for you: alpha, beta, gamma and two more."},
	} {
		maps.build = func(context.Context, string, []string) ([]workmap.Piece, error) {
			var p []workmap.Piece
			for _, name := range tc.names {
				p = append(p, workmap.Piece{Title: name, Stuck: "blocked"})
			}
			return p, nil
		}
		r := httptest.NewRequest("GET", "http://localhost/", nil)
		r.RemoteAddr = "127.0.0.1:1234"
		got := (&server{mapConfig: maps}).newBotWatcher(r).lookMap()
		if len(got) != 1 || got[0].Text != tc.want {
			t.Fatalf("got %+v want %s", got, tc.want)
		}
	}
}

func TestBotPageSummaryCounts(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{{1, "One piece waits for you."}, {4, "Four pieces wait for you."}} {
		s := &server{}
		s.mapCache.at = time.Now()
		for i := 0; i < tc.n; i++ {
			s.mapCache.pieces = append(s.mapCache.pieces, workmap.Piece{Stuck: "blocked"})
		}
		r := httptest.NewRequest("GET", "http://localhost/bot", nil)
		r.RemoteAddr = "127.0.0.1:1234"
		w := httptest.NewRecorder()
		s.bot(w, r)
		if !strings.Contains(w.Body.String(), tc.want) {
			t.Fatalf("missing %q", tc.want)
		}
	}
}
