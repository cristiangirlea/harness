package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/eval"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// agedRun makes a finished run whose id is days old.
func agedRun(t *testing.T, days int, suffix string) string {
	t.Helper()
	id := time.Now().AddDate(0, 0, -days).Format("20060102-150405") + "-" + suffix
	writeJSONFile(t, filepath.Join(runlog.Root(), id, "result.json"), map[string]string{"id": id})
	return id
}

func agedBatch(t *testing.T, id string, days int, runID, worktree string) {
	t.Helper()
	res := batch.Result{ID: id, Started: time.Now().AddDate(0, 0, -days), Tasks: []batch.TaskResult{{
		ID: "t1", Worktree: worktree, Attempts: []batch.Attempt{{Backend: "claude", RunID: runID}},
	}}}
	writeJSONFile(t, filepath.Join(runlog.Home(), "batches", id+".json"), res)
}

func agedEval(t *testing.T, id string, days int, batchID string) {
	t.Helper()
	writeJSONFile(t, filepath.Join(runlog.Home(), "evals", id+".json"),
		eval.Record{ID: id, BatchID: batchID, Started: time.Now().AddDate(0, 0, -days)})
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestPruneFollowsWhatKeptRecordsCite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	liveWorktree := t.TempDir() // a batch whose worktrees were never removed

	runOfOldBatch := agedRun(t, 40, "aaaaaa")
	runOfCitedBatch := agedRun(t, 40, "bbbbbb")
	runOfLiveBatch := agedRun(t, 40, "cccccc")
	freshRun := agedRun(t, 1, "dddddd")

	const oldB, citedB, liveB = "20260801-000000-b1b1b1", "20260801-000000-b2b2b2", "20260801-000000-b3b3b3"
	agedBatch(t, oldB, 40, runOfOldBatch, filepath.Join(home, "gone"))
	agedBatch(t, citedB, 40, runOfCitedBatch, filepath.Join(home, "gone"))
	agedBatch(t, liveB, 40, runOfLiveBatch, liveWorktree)
	agedEval(t, "e1old", 40, oldB)
	agedEval(t, "e2new", 1, citedB)

	path := func(parts ...string) string { return filepath.Join(append([]string{home}, parts...)...) }
	all := map[string]string{
		"run of the old batch":   path("runs", runOfOldBatch),
		"run of the cited batch": path("runs", runOfCitedBatch),
		"run of the live batch":  path("runs", runOfLiveBatch),
		"fresh run":              path("runs", freshRun),
		"old batch":              path("batches", oldB+".json"),
		"cited batch":            path("batches", citedB+".json"),
		"live batch":             path("batches", liveB+".json"),
		"old eval":               path("evals", "e1old.json"),
		"new eval":               path("evals", "e2new.json"),
	}

	if err := cmdPrune([]string{"--older-than", "30d", "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	for name, p := range all {
		if !pathExists(p) {
			t.Fatalf("--dry-run removed the %s", name)
		}
	}

	if err := cmdPrune([]string{"--older-than", "30d"}); err != nil {
		t.Fatal(err)
	}
	gone := map[string]bool{"run of the old batch": true, "old batch": true, "old eval": true}
	for name, p := range all {
		if got, want := pathExists(p), !gone[name]; got != want {
			t.Errorf("%s exists = %v, want %v", name, got, want)
		}
	}
}

func TestPruneRefusesWithoutAnAge(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, args := range [][]string{nil, {"--older-than", "30x"}, {"--older-than", "-5d"}, {"--older-than", "0d"}, {"--older-than", "30d", "--keep", "-1"}, {"--older-than", "30d", "extra"}} {
		if err := cmdPrune(args); err == nil {
			t.Errorf("%v: want an error", args)
		}
	}
	if err := cmdPrune(nil); err == nil || !strings.Contains(err.Error(), "--older-than") {
		t.Errorf("the refusal should name the flag: %v", err)
	}
}

func TestParseAge(t *testing.T) {
	for in, want := range map[string]time.Duration{"30d": 30 * 24 * time.Hour, "1d": 24 * time.Hour, "36h": 36 * time.Hour, "90m": 90 * time.Minute} {
		if got, err := parseAge(in); err != nil || got != want {
			t.Errorf("parseAge(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
}

func TestPruneRefusesWhileARecordCannotBeRead(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	run := agedRun(t, 40, "aaaaaa")
	// A record that cannot be read may cite that run; prune cannot know.
	broken := filepath.Join(home, "batches", "20260801-000000-b1b1b1.json")
	if err := os.MkdirAll(filepath.Dir(broken), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(broken, []byte("{half a rec"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := cmdPrune([]string{"--older-than", "30d"})
	if err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("err = %v, want a refusal naming the unreadable record", err)
	}
	if !pathExists(filepath.Join(runlog.Root(), run)) {
		t.Error("nothing may be removed while a record cannot be read")
	}
}

func TestPruneKeepsTheJudgeRunsOfAKeptEval(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	keptJudge := agedRun(t, 40, "aaaaaa")
	oldJudge := agedRun(t, 40, "bbbbbb")
	writeJSONFile(t, filepath.Join(home, "evals", "e1new.json"), eval.Record{ID: "e1new", Started: time.Now(),
		Judgements: []eval.Judgement{{Cell: "c1", RunID: keptJudge}}})
	writeJSONFile(t, filepath.Join(home, "evals", "e2old.json"), eval.Record{ID: "e2old", Started: time.Now().AddDate(0, 0, -40),
		Judgements: []eval.Judgement{{Cell: "c1", RunID: oldJudge}}})

	if err := cmdPrune([]string{"--older-than", "30d"}); err != nil {
		t.Fatal(err)
	}
	if !pathExists(filepath.Join(runlog.Root(), keptJudge)) {
		t.Error("a kept eval's judge run must stay: its verdict cites it")
	}
	if pathExists(filepath.Join(runlog.Root(), oldJudge)) {
		t.Error("the judge run of a removed eval should go with it")
	}
}
