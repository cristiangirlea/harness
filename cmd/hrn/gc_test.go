package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/gc"
)

func TestGCListsRemovalsFirstWithTheirSizeAndPullRequest(t *testing.T) {
	pr := &gc.PR{Number: 44, URL: "https://github.com/o/r/pull/44"}
	wts := []gc.Worktree{
		{Path: "/w/open", Branch: "going", Verdict: gc.Keep, Reason: "PR #45 is open", PR: &gc.PR{Number: 45, URL: "https://github.com/o/r/pull/45"}},
		{Path: "/w/done", Branch: "done", Verdict: gc.Remove, Reason: "PR #44 merged 2026-10-11 09:00", PR: pr},
		{Path: "/w/task", Batch: "20261011-100000-aaaaaa", Verdict: gc.Keep, Reason: "batch is still running"},
	}
	var out bytes.Buffer
	printGC(&out, wts, map[string]int64{"/w/done": 3 << 20})
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("want one line per worktree, got:\n%s", out.String())
	}
	for _, want := range []string{"remove", "/w/done", "done", "3.0 MB", "PR #44 merged", "https://github.com/o/r/pull/44"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("first line %q lacks %q", lines[0], want)
		}
	}
	if !strings.Contains(lines[1], "https://github.com/o/r/pull/45") || !strings.HasPrefix(lines[1], "keep") {
		t.Errorf("a kept worktree should show its pull request too: %q", lines[1])
	}
	if !strings.Contains(lines[2], "batch 20261011-100000-aaaaaa") {
		t.Errorf("a batch worktree should name its batch: %q", lines[2])
	}
}

func TestGCRefusesANegativeGrace(t *testing.T) {
	code, err := cmdGC([]string{"--grace", "-1h"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "--grace") {
		t.Errorf("got code %d, error %v; want 2 and an error naming --grace", code, err)
	}
}
