package agent

import "testing"

func TestWithDefaultNamesTheDefaultBackend(t *testing.T) {
	if got := WithDefault("", "claude"); got != "default backend claude was used" {
		t.Fatalf("got %q", got)
	}
	if got := WithDefault("no route row for purpose plan", "codex"); got != "no route row for purpose plan; default backend codex was used" {
		t.Fatalf("got %q", got)
	}
}

func TestContinuationNoteSaysWhyALimitMovedIt(t *testing.T) {
	if got := ContinuationNote("r1", ""); got != "" {
		t.Fatalf("a continuation on its own backend needs no note, got %q", got)
	}
	if got := ContinuationNote("r1", "codex is at its limit; carrying on on claude"); got != "continues run r1; codex is at its limit; carrying on on claude" {
		t.Fatalf("got %q", got)
	}
}
