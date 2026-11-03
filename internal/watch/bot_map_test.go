package watch

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

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
	if got := b.lookMap(); len(got) != 0 {
		t.Fatalf("error: %+v", got)
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
