package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

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

func TestGCListsCachesWithTheirSizeAndLastBuild(t *testing.T) {
	built := time.Date(2026, 10, 9, 21, 14, 0, 0, time.Local)
	cs := []gc.Cache{
		{Path: "/w/open/target", Worktree: "/w/open", Size: 2 << 30, LastBuild: built.Add(40 * time.Hour), Verdict: gc.Keep, Reason: "built 40m ago, inside --idle 24h"},
		{Path: "/d/sec-target", Size: 12 << 30, LastBuild: built, Verdict: gc.Remove, Reason: "idle 72h, last written 2026-10-09 21:14"},
	}
	var out bytes.Buffer
	printCaches(&out, cs)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want one line per cache, got:\n%s", out.String())
	}
	for _, want := range []string{"remove", "/d/sec-target", "cache", "12.0 GB", "idle 72h", "2026-10-09 21:14"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("first line %q lacks %q", lines[0], want)
		}
	}
	if !strings.HasPrefix(lines[1], "keep") || !strings.Contains(lines[1], "2.0 GB") {
		t.Errorf("a kept cache shows its size too: %q", lines[1])
	}
}

func TestGCSummaryCountsWorktreesAndCaches(t *testing.T) {
	wts := []gc.Worktree{{Path: "/w/a", Verdict: gc.Remove}, {Path: "/w/b", Verdict: gc.Keep}}
	cs := []gc.Cache{{Path: "/c/x", Size: 2 << 20, Verdict: gc.Remove}, {Path: "/c/y", Verdict: gc.Keep}, {Path: "/c/z", Verdict: gc.Keep}}
	got := gcSummary(wts, cs, map[string]int64{"/w/a": 1 << 20})
	want := "would remove 1 of 2 worktrees and 1 of 3 caches, freeing 3.0 MB; hrn gc --apply removes them"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	for i := range wts {
		wts[i].Verdict = gc.Keep
	}
	cs[0].Verdict = gc.Keep
	if got, want := gcSummary(wts, cs, nil), "nothing to remove; 2 worktrees and 3 caches stay"; got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestParseSizeReadsHumanUnits(t *testing.T) {
	for in, want := range map[string]int64{"": 0, "123": 123, "2k": 2 << 10, "500M": 500 << 20, "50G": 50 << 30, "50GB": 50 << 30, "1T": 1 << 40, "1.5G": 3 << 29} {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"lots", "-1G", "G", "1X", "99999999999T"} {
		if _, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) accepted", in)
		}
	}
}

func TestGCAcceptsAnIdleBelowTheDefaultMinIdle(t *testing.T) {
	code, err := cmdGC([]string{"--idle", "1h", t.TempDir()})
	if code != 0 || err != nil {
		t.Errorf("--idle 1h with the default --min-idle: got code %d, error %v", code, err)
	}
}

func TestGCRefusesBadIdleFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--idle", "-1h"},
		{"--min-idle", "-5m"},
		{"--free-below", "lots"},
	} {
		code, err := cmdGC(args)
		if code != 2 || err == nil || !strings.Contains(err.Error(), "--") {
			t.Errorf("%v: got code %d, error %v; want 2 and an error naming the flag", args, code, err)
		}
	}
}
