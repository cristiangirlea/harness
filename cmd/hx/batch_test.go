package main

import (
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
)

func TestPrintBatchTable(t *testing.T) {
	var out strings.Builder
	r := newRenderer(false)
	r.out, r.color = &out, false
	printBatch(r, batch.Result{ID: "b1", Dir: "/repo", Duration: 90 * time.Second, Tasks: []batch.TaskResult{
		{ID: "done", Status: batch.StatusPass, Branch: "hx/done", Attempts: []batch.Attempt{{Backend: "api"}}},
		{ID: "going", Status: batch.StatusRunning, Branch: "hx/going"},
		{ID: "failed", Status: batch.StatusFail, Branch: "hx/failed", Attempts: []batch.Attempt{{Backend: "claude"}, {Backend: "claude"}}},
	}})
	text := out.String()
	for _, want := range []string{
		"done", "pass", "hx/done", "1 attempt(s)",
		"going", "running",
		"failed", "2 attempt(s)",
		"1/3 passed", "1 still running", "hx batch merge b1", "hx batch rm b1",
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
		Tasks: []batch.TaskResult{{ID: "going", Status: batch.StatusRunning, Branch: "hx/going"}}})
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
