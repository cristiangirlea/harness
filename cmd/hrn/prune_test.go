package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/eval"
	"github.com/cristiangirlea/harness/internal/game"
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

// oldRecords makes a review run, a batch with a squashed bug and an eval, all 40 days old.
func oldRecords(t *testing.T, home string) (runID, batchID string, paths []string) {
	t.Helper()
	started := time.Now().AddDate(0, 0, -40)
	runID = started.Format("20060102-150405") + "-aaaaaa"
	batchID = started.Format("20060102-150405") + "-b1b1b1"
	writeJSONFile(t, filepath.Join(runlog.Root(), runID, "result.json"), runlog.Record{
		ID: runID, Backend: "claude", Started: started,
		Task:   agent.Task{Prompt: "review the branch", Purpose: "review", Dir: home},
		Result: agent.Result{Backend: "claude", Status: agent.StatusOK, Final: "No defects."},
	})
	writeJSONFile(t, filepath.Join(runlog.Home(), "batches", batchID+".json"), batch.Result{ID: batchID, Started: started, Dir: home,
		Tasks: []batch.TaskResult{{ID: "t1", Status: batch.StatusPass, Worktree: filepath.Join(home, "gone"), Attempts: []batch.Attempt{
			{Backend: "claude", Gates: []batch.GateResult{{Command: "go test ./...", ExitCode: 1}}},
			{Backend: "claude", Gates: []batch.GateResult{{Command: "go test ./...", ExitCode: 0}}},
		}}}})
	agedEval(t, "e1old", 40, batchID)
	return runID, batchID, []string{
		filepath.Join(runlog.Root(), runID),
		filepath.Join(runlog.Home(), "batches", batchID+".json"),
		filepath.Join(runlog.Home(), "evals", "e1old.json"),
	}
}

func TestPruneFoldsWhatItDeletesIntoTheLedgerFirst(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	runID, batchID, paths := oldRecords(t, home)
	if _, err := captureStdout(t, func() error { return cmdPrune([]string{"--older-than", "30d"}) }); err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if pathExists(p) {
			t.Errorf("%s was not pruned", p)
		}
	}
	events, _, err := game.Load()
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, e := range events {
		have[e.Key] = true
	}
	for _, key := range []string{"review:" + runID, "batch:" + batchID + "/t1", "first:batch", "first:eval"} {
		if !have[key] {
			t.Errorf("the ledger lacks %s after the prune; it holds %v", key, have)
		}
	}
}

func TestPruneDeletesNothingWhenTheLedgerCannotTakeTheRecords(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	_, _, paths := oldRecords(t, home)
	// A file where the game directory belongs: the ledger cannot be written.
	if err := os.WriteFile(game.Dir(), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := captureStdout(t, func() error { return cmdPrune([]string{"--older-than", "30d"}) })
	if err == nil || !strings.Contains(err.Error(), "ledger") || !strings.Contains(err.Error(), "nothing was removed") {
		t.Fatalf("err = %v, want a refusal saying the ledger failed and nothing was removed", err)
	}
	for _, p := range paths {
		if !pathExists(p) {
			t.Errorf("%s was removed although the ledger could not take it", p)
		}
	}
}

func TestPruneDryRunLeavesTheLedgerAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	_, _, paths := oldRecords(t, home)
	if _, err := captureStdout(t, func() error { return cmdPrune([]string{"--older-than", "30d", "--dry-run"}) }); err != nil {
		t.Fatal(err)
	}
	if pathExists(game.Dir()) {
		t.Error("a dry run wrote the ledger")
	}
	for _, p := range paths {
		if !pathExists(p) {
			t.Errorf("a dry run removed %s", p)
		}
	}
}

func ledgerEvent(t *testing.T, key string) (game.Event, bool) {
	t.Helper()
	events, _, err := game.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Key == key {
			return e, true
		}
	}
	return game.Event{}, false
}

func TestPruneKeepsTheVerdictOfARoutedRunWhoseBatchGoes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	started := time.Now().AddDate(0, 0, -40)
	runID := started.Format("20060102-150405") + "-aaaaaa"
	writeJSONFile(t, filepath.Join(runlog.Root(), runID, "result.json"), runlog.Record{
		ID: runID, Backend: "claude", Started: started, RoutedBy: &runlog.RoutedBy{Category: "implement", Set: "2026-10-19"},
		Task:   agent.Task{Prompt: "implement it", Dir: home},
		Result: agent.Result{Backend: "claude", Status: agent.StatusOK},
	})
	batchID := started.Format("20060102-150405") + "-b1b1b1"
	writeJSONFile(t, filepath.Join(runlog.Home(), "batches", batchID+".json"), batch.Result{ID: batchID, Started: started, Dir: home,
		Tasks: []batch.TaskResult{{ID: "t1", Status: batch.StatusFail, Worktree: filepath.Join(home, "gone"), Attempts: []batch.Attempt{
			{Backend: "claude", RunID: runID, Gates: []batch.GateResult{{Command: "go test ./...", ExitCode: 1}}},
		}}}})
	// --keep 1 keeps the run while its batch record goes.
	if _, err := captureStdout(t, func() error { return cmdPrune([]string{"--older-than", "30d", "--keep", "1"}) }); err != nil {
		t.Fatal(err)
	}
	if pathExists(filepath.Join(runlog.Home(), "batches", batchID+".json")) || !pathExists(filepath.Join(runlog.Root(), runID)) {
		t.Fatal("want the batch record pruned and the run kept")
	}
	if err := game.Rebuild(); err != nil {
		t.Fatal(err)
	}
	if e, ok := ledgerEvent(t, "routed:"+runID); !ok || e.Kind != "routed_failed" || e.XP != 0 {
		t.Fatalf("routed:%s = %+v (%v); want the failed verdict kept, so the run never counts once its batch is gone", runID, e, ok)
	}
}

func TestPruneDatesFirstUseByTheEarliestRecordOnDisk(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	const keptB, goneB = "20260801-000000-b1b1b1", "20260802-000000-b2b2b2"
	agedBatch(t, keptB, 40, "", filepath.Join(home, "gone"))
	agedBatch(t, goneB, 35, "", filepath.Join(home, "gone"))
	agedEval(t, "e2new", 1, keptB) // keeps the older batch
	if _, err := captureStdout(t, func() error { return cmdPrune([]string{"--older-than", "30d"}) }); err != nil {
		t.Fatal(err)
	}
	if pathExists(filepath.Join(runlog.Home(), "batches", goneB+".json")) || !pathExists(filepath.Join(runlog.Home(), "batches", keptB+".json")) {
		t.Fatal("want the newer old batch pruned and the cited one kept")
	}
	kept, err := batch.LoadResult(keptB)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := ledgerEvent(t, "first:batch"); !ok || !e.At.Equal(kept.Started) {
		t.Fatalf("first:batch = %+v (%v); want it dated at the earliest batch on disk, %v", e, ok, kept.Started)
	}
}

func TestPruneRefusesWhenARunItWouldRemoveCannotBeRead(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	id := time.Now().AddDate(0, 0, -40).Format("20060102-150405") + "-aaaaaa"
	// A directory where result.json belongs: the record exists but cannot be read.
	if err := os.MkdirAll(filepath.Join(runlog.Root(), id, "result.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := captureStdout(t, func() error { return cmdPrune([]string{"--older-than", "30d"}) })
	if err == nil || !strings.Contains(err.Error(), "nothing was removed") {
		t.Fatalf("err = %v, want a refusal that removed nothing", err)
	}
	if !pathExists(filepath.Join(runlog.Root(), id)) {
		t.Error("a run that could not be read was removed without being folded")
	}
}

func TestPruneDecidesTheRoutedRunOfABatchThatDiedAsFailed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	started := time.Now().AddDate(0, 0, -40)
	runID := started.Format("20060102-150405") + "-aaaaaa"
	writeJSONFile(t, filepath.Join(runlog.Root(), runID, "result.json"), runlog.Record{
		ID: runID, Backend: "claude", Started: started, RoutedBy: &runlog.RoutedBy{Category: "implement", Set: "2026-10-19"},
		Task:   agent.Task{Prompt: "implement it", Dir: home},
		Result: agent.Result{Backend: "claude", Status: agent.StatusOK},
	})
	// The batch process died while the attempt's gates ran: the task is still "running" in its record.
	batchID := started.Format("20060102-150405") + "-b1b1b1"
	writeJSONFile(t, filepath.Join(runlog.Home(), "batches", batchID+".json"), batch.Result{ID: batchID, Started: started, Dir: home,
		Tasks: []batch.TaskResult{{ID: "t1", Status: batch.StatusRunning, Worktree: filepath.Join(home, "gone"), Attempts: []batch.Attempt{
			{Backend: "claude", RunID: runID, Gates: []batch.GateResult{{Command: "go vet ./...", ExitCode: 0}}},
		}}}})
	if _, err := captureStdout(t, func() error { return cmdPrune([]string{"--older-than", "30d", "--keep", "1"}) }); err != nil {
		t.Fatal(err)
	}
	if pathExists(filepath.Join(runlog.Home(), "batches", batchID+".json")) || !pathExists(filepath.Join(runlog.Root(), runID)) {
		t.Fatal("want the batch record pruned and the run kept")
	}
	if err := game.Rebuild(); err != nil {
		t.Fatal(err)
	}
	if e, ok := ledgerEvent(t, "routed:"+runID); !ok || e.Kind != "routed_failed" {
		t.Fatalf("routed:%s = %+v (%v); want routed_failed, decided before the record went", runID, e, ok)
	}
}

func TestPruneRemovesRunsWhoseRecordHoldsNothingToFold(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	old := time.Now().AddDate(0, 0, -40)
	id := func(suffix string) string { return old.Format("20060102-150405") + "-" + suffix }
	broken, mistyped, empty, good := id("aaaaaa"), id("bbbbbb"), id("cccccc"), id("dddddd")
	for run, body := range map[string]string{broken: "{half a rec", mistyped: `{"id": 5}`} {
		if err := os.MkdirAll(filepath.Join(runlog.Root(), run), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runlog.Root(), run, "result.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A run that never wrote task.json or result.json, its transcript long untouched.
	events := filepath.Join(runlog.Root(), empty, "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(events), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(events, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(events, old, old); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(runlog.Root(), good, "result.json"), runlog.Record{
		ID: good, Backend: "claude", Started: old,
		Task:   agent.Task{Prompt: "review the branch", Purpose: "review", Dir: home},
		Result: agent.Result{Backend: "claude", Status: agent.StatusOK, Final: "No defects."},
	})
	if _, err := captureStdout(t, func() error { return cmdPrune([]string{"--older-than", "30d"}) }); err != nil {
		t.Fatal(err)
	}
	for _, run := range []string{broken, mistyped, empty, good} {
		if pathExists(filepath.Join(runlog.Root(), run)) {
			t.Errorf("run %s was kept; a record with nothing to fold may go", run)
		}
	}
	if _, ok := ledgerEvent(t, "review:"+good); !ok {
		t.Error("the readable run was not folded before it went")
	}
}
