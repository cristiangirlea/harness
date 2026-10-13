package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestPrintBatchTable(t *testing.T) {
	var out strings.Builder
	r := newRenderer(false)
	r.out, r.color = &out, false
	printBatch(r, batch.Result{ID: "b1", Dir: "/repo", Duration: 90 * time.Second, Tasks: []batch.TaskResult{
		{ID: "done", Status: batch.StatusPass, Branch: "hrn/done", Attempts: []batch.Attempt{{Backend: "api"}}},
		{ID: "going", Status: batch.StatusRunning, Branch: "hrn/going"},
		{ID: "failed", Status: batch.StatusFail, Branch: "hrn/failed", Attempts: []batch.Attempt{{Backend: "claude"}, {Backend: "claude"}}},
	}})
	text := out.String()
	for _, want := range []string{
		"done", "pass", "hrn/done", "1 attempt(s)",
		"going", "running",
		"failed", "2 attempt(s)",
		"1/3 passed", "1 still running", "hrn batch merge b1", "hrn batch rm b1",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("table is missing %q:\n%s", want, text)
		}
	}
}

func TestPrintBatchTimesARunningBatchFromItsStart(t *testing.T) {
	var out strings.Builder
	r := newRenderer(false)
	r.out, r.color = &out, false
	printBatch(r, batch.Result{ID: "b3", Started: time.Now().Add(-3 * time.Minute),
		Tasks: []batch.TaskResult{{ID: "going", Status: batch.StatusRunning, Branch: "hrn/going"}}})
	text := out.String()
	if !strings.Contains(text, "3m0s so far") {
		t.Errorf("a running batch should be timed from its start:\n%s", text)
	}
	if strings.Contains(text, " 0s") {
		t.Errorf("a running task has no duration to print:\n%s", text)
	}
}

func TestPrintBatchSaysNothingAboutRunningWhenNoneAre(t *testing.T) {
	var out strings.Builder
	r := newRenderer(false)
	r.out, r.color = &out, false
	printBatch(r, batch.Result{ID: "b2", Tasks: []batch.TaskResult{{ID: "one", Status: batch.StatusPass}}})
	if text := out.String(); strings.Contains(text, "still running") || !strings.Contains(text, "1/1 passed") {
		t.Errorf("table = %s", text)
	}
}

func TestShowHeaderSaysWhenTheRunRecordIsIncomplete(t *testing.T) {
	for _, tc := range []struct {
		recordError string
		want        bool
	}{
		{"2 events were not recorded: write events.jsonl: no space left on device", true},
		{"", false},
	} {
		var out strings.Builder
		r := newRenderer(false)
		r.out, r.color = &out, false
		showHeader(r, runlog.Record{ID: "r1", Backend: "codex", RecordError: tc.recordError})
		got := strings.Contains(out.String(), "run record incomplete: 2 events were not recorded")
		if got != tc.want {
			t.Errorf("record_error %q: header mentions it = %v, want %v:\n%s", tc.recordError, got, tc.want, out.String())
		}
	}
}

func TestPrintBatchNamesTheSlotsOrTheFreshWorktrees(t *testing.T) {
	print := func(res batch.Result) string {
		var out strings.Builder
		r := newRenderer(false)
		r.out, r.color = &out, false
		printBatch(r, res)
		return out.String()
	}
	slot := filepath.Join("main", ".hrn", "slots", "2")
	text := print(batch.Result{ID: "b4", Dir: "repo", Tasks: []batch.TaskResult{
		{ID: "one", Status: batch.StatusPass, Branch: "hrn/one", Worktree: slot, Slot: 2}}})
	if !strings.Contains(text, filepath.Join("main", ".hrn", "slots")) || strings.Contains(text, filepath.Join(".hrn", "worktrees")) ||
		!strings.Contains(text, "hrn batch rm b4") {
		t.Errorf("a slot batch's hint should name the slots, not .hrn/worktrees:\n%s", text)
	}
	fresh := filepath.Join("repo", ".hrn", "worktrees", "one")
	text = print(batch.Result{ID: "b5", Dir: "repo", Tasks: []batch.TaskResult{
		{ID: "one", Status: batch.StatusPass, Branch: "hrn/one", Worktree: fresh}}})
	if !strings.Contains(text, "worktrees under "+filepath.Join("repo", ".hrn", "worktrees")) {
		t.Errorf("a fresh batch's hint names its worktrees:\n%s", text)
	}
}
