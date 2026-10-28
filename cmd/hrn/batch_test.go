package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"encoding/json"
	"fmt"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestBatchMergeRunsAutomaticGCOnlyWhenEnabled(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			res := batchMergeFixture(t, false, "alpha")
			b, _ := json.Marshal(map[string]any{"enabled": enabled, "roots": []string{res.Dir}})
			if err := os.WriteFile(filepath.Join(os.Getenv("HRN_HOME"), "gc.json"), b, 0600); err != nil {
				t.Fatal(err)
			}
			code, _, err := batchMergeCLI(t, res.ID)
			if code != 0 || err != nil {
				t.Fatalf("merge: %d %v", code, err)
			}
			_, err = os.Stat(filepath.Join(os.Getenv("HRN_HOME"), "gc.log"))
			if enabled && err != nil || !enabled && !os.IsNotExist(err) {
				t.Fatalf("enabled %v: log error %v", enabled, err)
			}
		})
	}
}

func TestBatchMergeSucceedsWhenDisabledGCConfigHasInvalidFields(t *testing.T) {
	res := batchMergeFixture(t, false, "alpha")
	os.WriteFile(filepath.Join(os.Getenv("HRN_HOME"), "gc.json"), []byte(`{"enabled":false,"interval":-5,"roots":"invalid"}`), 0600)
	if code, _, err := batchMergeCLI(t, res.ID); code != 0 || err != nil {
		t.Fatalf("merge %d: %v", code, err)
	}
}

func batchMergeGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func batchMergeFixture(t *testing.T, sameFile bool, ids ...string) batch.Result {
	t.Helper()
	t.Setenv("HRN_HOME", t.TempDir())
	dir := t.TempDir()
	batchMergeGit(t, dir, "init", "-q", "-b", "main")
	batchMergeGit(t, dir, "config", "user.email", "test@example.com")
	batchMergeGit(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	batchMergeGit(t, dir, "add", "README.md")
	batchMergeGit(t, dir, "commit", "-q", "-m", "base")
	res := batch.Result{ID: "b1", Dir: dir, Base: batchMergeGit(t, dir, "rev-parse", "HEAD"), Ownership: batch.OwnershipClaim}
	for _, id := range ids {
		branch := "hrn/" + id
		wt := filepath.Join(t.TempDir(), id)
		batchMergeGit(t, dir, "worktree", "add", "-q", "-b", branch, wt)
		name := id + ".txt"
		if sameFile {
			name = "out.txt"
		}
		if err := os.WriteFile(filepath.Join(wt, name), []byte(id+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		batchMergeGit(t, wt, "add", name)
		batchMergeGit(t, wt, "commit", "-q", "-m", id)
		res.Tasks = append(res.Tasks, batch.TaskResult{ID: id, Prompt: id, Status: batch.StatusPass,
			Branch: branch, Worktree: wt, BranchMade: true, Commit: batchMergeGit(t, wt, "rev-parse", "HEAD")})
	}
	if err := batch.SaveResult(res); err != nil {
		t.Fatal(err)
	}
	return res
}

func TestBatchMergeFixtureSavesClaimedRecord(t *testing.T) {
	res := batchMergeFixture(t, false, "alpha")
	saved, err := batch.LoadResult(res.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Ownership != batch.OwnershipClaim {
		t.Fatalf("saved ownership = %d, want %d", saved.Ownership, batch.OwnershipClaim)
	}
}

func batchMergeCLI(t *testing.T, args ...string) (int, string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	code, runErr := cmdBatchMerge(args)
	_ = w.Close()
	out, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	return code, string(out), runErr
}

func TestBatchMergeRmRemovesOnlyTasksMergedInThisCall(t *testing.T) {
	res := batchMergeFixture(t, false, "alpha", "beta")
	code, out, err := batchMergeCLI(t, "--rm", "--squash", res.ID, "alpha")
	if code != 0 || err != nil || !strings.Contains(out, "removed the worktrees and branches of 1 merged task(s)") ||
		!strings.Contains(out, "only in the reflog") || strings.Contains(out, "hrn batch rm") {
		t.Fatalf("code=%d err=%v output=%q", code, err, out)
	}
	if _, err := os.Stat(res.Tasks[0].Worktree); !os.IsNotExist(err) {
		t.Errorf("merged worktree remains: %v", err)
	}
	if branch := batchMergeGit(t, res.Dir, "branch", "--list", res.Tasks[0].Branch); branch != "" {
		t.Errorf("squash-merged branch remains: %s", branch)
	}
	if _, err := os.Stat(res.Tasks[1].Worktree); err != nil {
		t.Errorf("unmerged worktree is gone: %v", err)
	}
	if branch := batchMergeGit(t, res.Dir, "branch", "--list", res.Tasks[1].Branch); branch == "" {
		t.Error("unmerged branch is gone")
	}
}

func TestBatchMergeRmUndoAfterConflictUsesPreMergeHead(t *testing.T) {
	res := batchMergeFixture(t, true, "alpha", "beta")
	before := batchMergeGit(t, res.Dir, "rev-parse", "HEAD")
	code, out, err := batchMergeCLI(t, "--rm", res.ID)
	if code != 1 || err == nil || !strings.Contains(out, "conflict merging") ||
		!strings.Contains(out, "removed the worktrees and branches of 1 merged task(s)") ||
		!strings.Contains(out, "undo with: git reset --hard "+before) {
		t.Fatalf("code=%d err=%v output=%q", code, err, out)
	}
	batchMergeGit(t, res.Dir, "reset", "--hard", before)
	if head := batchMergeGit(t, res.Dir, "rev-parse", "HEAD"); head != before {
		t.Errorf("reset HEAD = %s, want %s", head, before)
	}
	if _, err := os.Stat(res.Tasks[0].Worktree); !os.IsNotExist(err) {
		t.Errorf("earlier merged worktree remains: %v", err)
	}
	if branch := batchMergeGit(t, res.Dir, "branch", "--list", res.Tasks[0].Branch); branch != "" {
		t.Errorf("earlier merged branch remains: %s", branch)
	}
	if _, err := os.Stat(res.Tasks[1].Worktree); err != nil {
		t.Errorf("conflicting worktree is gone: %v", err)
	}
	if branch := batchMergeGit(t, res.Dir, "branch", "--list", res.Tasks[1].Branch); branch == "" {
		t.Error("conflicting branch is gone")
	}
}

func TestBatchMergeRmLeavesSkippedFailedAndAlreadyMergedTasks(t *testing.T) {
	res := batchMergeFixture(t, false, "alpha", "beta")
	res.Tasks[1].Status = batch.StatusFail
	if err := batch.SaveResult(res); err != nil {
		t.Fatal(err)
	}
	code, _, err := batchMergeCLI(t, res.ID, "alpha")
	if code != 0 || err != nil {
		t.Fatalf("initial merge: code=%d err=%v", code, err)
	}
	code, out, err := batchMergeCLI(t, "--rm", res.ID, "alpha", "beta")
	if code != 0 || err != nil || !strings.Contains(out, "already merged") || !strings.Contains(out, "did not pass") ||
		strings.Contains(out, "removed the") {
		t.Fatalf("code=%d err=%v output=%q", code, err, out)
	}
	for _, tr := range res.Tasks {
		if _, err := os.Stat(tr.Worktree); err != nil {
			t.Errorf("skipped task %s lost its worktree: %v", tr.ID, err)
		}
		if branch := batchMergeGit(t, res.Dir, "branch", "--list", tr.Branch); branch == "" {
			t.Errorf("skipped task %s lost its branch", tr.ID)
		}
	}
}

func TestBatchMergeWithoutRmKeepsTaskWorktreeAndBranch(t *testing.T) {
	res := batchMergeFixture(t, false, "alpha")
	code, out, err := batchMergeCLI(t, res.ID)
	if code != 0 || err != nil || !strings.Contains(out, "hrn batch rm") {
		t.Fatalf("code=%d err=%v output=%q", code, err, out)
	}
	if _, err := os.Stat(res.Tasks[0].Worktree); err != nil {
		t.Errorf("worktree is gone: %v", err)
	}
	if branch := batchMergeGit(t, res.Dir, "branch", "--list", res.Tasks[0].Branch); branch == "" {
		t.Error("branch is gone")
	}
}

func TestBatchMergeUndoAllTasksUsesPreMergeHead(t *testing.T) {
	res := batchMergeFixture(t, false, "alpha", "beta")
	before := batchMergeGit(t, res.Dir, "rev-parse", "HEAD")
	code, out, err := batchMergeCLI(t, res.ID)
	if code != 0 || err != nil || !strings.Contains(out, "2 branch(es) merged") ||
		!strings.Contains(out, "undo with: git reset --hard "+before) || strings.Contains(out, "ORIG_HEAD") {
		t.Fatalf("code=%d err=%v output=%q", code, err, out)
	}
	batchMergeGit(t, res.Dir, "reset", "--hard", before)
	if head := batchMergeGit(t, res.Dir, "rev-parse", "HEAD"); head != before {
		t.Errorf("reset HEAD = %s, want %s", head, before)
	}
	for _, name := range []string{"alpha.txt", "beta.txt"} {
		if _, err := os.Stat(filepath.Join(res.Dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s remains after reset: %v", name, err)
		}
	}
}

func TestBatchMergeUndoAfterConflictWithoutRmUsesPreMergeHead(t *testing.T) {
	res := batchMergeFixture(t, true, "alpha", "beta")
	before := batchMergeGit(t, res.Dir, "rev-parse", "HEAD")
	code, out, err := batchMergeCLI(t, res.ID)
	if code != 1 || err == nil || !strings.Contains(out, "conflict merging") ||
		!strings.Contains(out, "undo with: git reset --hard "+before) {
		t.Fatalf("code=%d err=%v output=%q", code, err, out)
	}
	if head := batchMergeGit(t, res.Dir, "rev-parse", "HEAD"); head == before {
		t.Fatal("earlier task was not merged before the conflict")
	}
}

func TestBatchMergeRmUndoWhenNothingRemoved(t *testing.T) {
	res := batchMergeFixture(t, false, "alpha")
	// Without BranchMade, a worktree claimed under ownership belongs to another
	// batch: --rm removes nothing of it and says nothing is wrong.
	res.Tasks[0].BranchMade = false
	if err := batch.SaveResult(res); err != nil {
		t.Fatal(err)
	}
	before := batchMergeGit(t, res.Dir, "rev-parse", "HEAD")
	code, out, err := batchMergeCLI(t, "--rm", res.ID)
	if code != 0 || err != nil || !strings.Contains(out, "undo with: git reset --hard "+before) ||
		strings.Contains(out, "removed the") || strings.Contains(out, "only in the reflog") {
		t.Fatalf("code=%d err=%v output=%q", code, err, out)
	}
	if _, err := os.Stat(res.Tasks[0].Worktree); err != nil {
		t.Errorf("another batch's worktree is gone: %v", err)
	}
	batchMergeGit(t, res.Dir, "rev-parse", "--verify", "refs/heads/"+res.Tasks[0].Branch)
}

func TestBatchMergeNothingMergedHasNoUndoHint(t *testing.T) {
	for _, rm := range []bool{false, true} {
		t.Run(map[bool]string{false: "keep", true: "rm"}[rm], func(t *testing.T) {
			res := batchMergeFixture(t, false, "alpha")
			args := []string{res.ID}
			if rm {
				args = append([]string{"--rm"}, args...)
			}
			code, _, err := batchMergeCLI(t, res.ID)
			if code != 0 || err != nil {
				t.Fatalf("initial merge: code=%d err=%v", code, err)
			}
			code, out, err := batchMergeCLI(t, args...)
			if code != 0 || err != nil || !strings.Contains(out, "already merged") || strings.Contains(out, "undo with:") {
				t.Fatalf("code=%d err=%v output=%q", code, err, out)
			}
		})
	}
}

func TestBatchMergeRmReportsDirtyMergedWorktree(t *testing.T) {
	res := batchMergeFixture(t, false, "alpha")
	if err := os.WriteFile(filepath.Join(res.Tasks[0].Worktree, "notes.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, err := batchMergeCLI(t, "--rm", res.ID)
	if code != 1 || err == nil || !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if _, err := os.Stat(res.Tasks[0].Worktree); err != nil {
		t.Errorf("dirty worktree is gone: %v", err)
	}
	if branch := batchMergeGit(t, res.Dir, "branch", "--list", res.Tasks[0].Branch); branch == "" {
		t.Error("dirty task branch is gone")
	}
}

func TestBatchMergeRmReportsOnlyRemovedTasksAfterDirtyFailure(t *testing.T) {
	res := batchMergeFixture(t, false, "alpha", "beta")
	before := batchMergeGit(t, res.Dir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(res.Tasks[1].Worktree, "notes.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, err := batchMergeCLI(t, "--rm", res.ID)
	if code != 1 || err == nil || !strings.Contains(err.Error(), "beta") ||
		strings.Contains(err.Error(), "alpha") || !strings.Contains(out, "removed the worktrees and branches of 1 merged task(s)") ||
		!strings.Contains(out, "undo with: git reset --hard "+before) {
		t.Fatalf("code=%d err=%v output=%q", code, err, out)
	}
	if _, err := os.Stat(res.Tasks[0].Worktree); !os.IsNotExist(err) {
		t.Errorf("clean worktree remains: %v", err)
	}
	if branch := batchMergeGit(t, res.Dir, "branch", "--list", res.Tasks[0].Branch); branch != "" {
		t.Errorf("clean branch remains: %s", branch)
	}
	if _, err := os.Stat(res.Tasks[1].Worktree); err != nil {
		t.Errorf("dirty worktree is gone: %v", err)
	}
	if branch := batchMergeGit(t, res.Dir, "branch", "--list", res.Tasks[1].Branch); branch == "" {
		t.Error("dirty branch is gone")
	}
}

func TestBatchMergeRmKeepsSlotDirectory(t *testing.T) {
	res := batchMergeFixture(t, false, "alpha")
	batchMergeGit(t, res.Dir, "worktree", "remove", res.Tasks[0].Worktree)
	slot := filepath.Join(t.TempDir(), "slot")
	if err := os.MkdirAll(slot, 0o755); err != nil {
		t.Fatal(err)
	}
	res.Tasks[0].Slot, res.Tasks[0].Worktree = 1, slot
	if err := batch.SaveResult(res); err != nil {
		t.Fatal(err)
	}
	code, out, err := batchMergeCLI(t, "--rm", res.ID)
	if code != 0 || err != nil || !strings.Contains(out, "removed the branches of 1 merged task(s)") {
		t.Fatalf("code=%d err=%v output=%q", code, err, out)
	}
	if _, err := os.Stat(slot); err != nil {
		t.Errorf("slot directory is gone: %v", err)
	}
	if branch := batchMergeGit(t, res.Dir, "branch", "--list", res.Tasks[0].Branch); branch != "" {
		t.Errorf("slot branch remains: %s", branch)
	}
}

func TestPrintBatchTable(t *testing.T) {
	var out strings.Builder
	r := newRenderer(false)
	r.out, r.color = &out, false
	printBatch(r, batch.Result{ID: "b1", Dir: "/repo", Duration: 90 * time.Second, Tasks: []batch.TaskResult{
		{ID: "done", Purpose: "review", Status: batch.StatusPass, Branch: "hrn/done", Attempts: []batch.Attempt{{Backend: "api"}}},
		{ID: "own", Status: batch.StatusPass, Branch: "hrn/own", Attempts: []batch.Attempt{{Backend: "codex", AgentCommitted: true}}},
		{ID: "going", Status: batch.StatusRunning, Branch: "hrn/going"},
		{ID: "failed", Status: batch.StatusFail, Branch: "hrn/failed", Attempts: []batch.Attempt{{Backend: "claude"}, {Backend: "claude"}}},
	}})
	text := out.String()
	if strings.Count(text, "agent committed") != 1 {
		t.Errorf("only the task whose agent committed is marked:\n%s", text)
	}
	for _, want := range []string{
		"done", "pass", "review", "hrn/done", "1 attempt(s)",
		"going", "running",
		"failed", "2 attempt(s)",
		"own", "agent committed",
		"2/4 passed", "1 still running", "hrn batch merge b1", "hrn batch rm b1",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("table is missing %q:\n%s", want, text)
		}
	}
}

func TestBatchShowDisplaysPassingAttemptNotes(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	note := "commit not recorded: the branch tip abc1234 does not contain the base"
	res := batch.Result{ID: "notes", Tasks: []batch.TaskResult{{
		ID: "one", Status: batch.StatusPass, Attempts: []batch.Attempt{{Backend: "stub", Error: note}},
	}}}
	if err := batch.SaveResult(res); err != nil {
		t.Fatal(err)
	}
	var table strings.Builder
	r := newRenderer(false)
	r.out, r.color = &table, false
	printBatch(r, res)
	if !strings.Contains(table.String(), "\n    attempt 1 note: "+note+"\n") {
		t.Errorf("table omitted the attempt note: %s", table.String())
	}
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = wr
	defer func() { os.Stdout = old }()
	code, showErr := cmdBatchShow([]string{res.ID})
	_ = wr.Close()
	out, err := io.ReadAll(rd)
	_ = rd.Close()
	if err != nil || showErr != nil || code != 0 || !strings.Contains(string(out), note) {
		t.Errorf("batch show = %d, %v, %v, %s: want the passing attempt note", code, showErr, err, out)
	}
}

func TestBatchAttemptNotesCannotSendTerminalControls(t *testing.T) {
	res := batch.Result{ID: "controls", Tasks: []batch.TaskResult{{
		ID: "one", Status: batch.StatusPass, Attempts: []batch.Attempt{{Backend: "stub", Error: "commit: \x1b]0;title\x07 \x1b[31mred\u009b"}},
	}}}
	var table strings.Builder
	r := newRenderer(false)
	r.out, r.color = &table, false
	printBatch(r, res)
	if out := table.String(); strings.ContainsAny(out, "\x1b\x07\u009b") || !strings.Contains(out, "attempt 1 note: commit:") {
		t.Errorf("the note reached the terminal with its control characters: %q", out)
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

func TestBatchRmAndMergeNameTheBranchesOfASlotBatchAndTheWorktreesOfAFreshOne(t *testing.T) {
	slots := batch.Result{Tasks: []batch.TaskResult{
		{ID: "one", Worktree: filepath.Join("repo", ".hrn", "slots", "1"), Slot: 1},
		{ID: "waited", Status: batch.StatusRunning}}}
	if got := removedWhat(slots); got != "branches" {
		t.Errorf("a slot batch: %q, want branches", got)
	}
	fresh := batch.Result{Tasks: []batch.TaskResult{{ID: "one", Worktree: filepath.Join("repo", ".hrn", "worktrees", "one")}}}
	if got := removedWhat(fresh); got != "worktrees and branches" {
		t.Errorf("a fresh batch: %q, want worktrees and branches", got)
	}
}

func TestBatchDiffOfASlotTaskThatCommittedNothingSaysSoAndWhetherItsSlotWasKept(t *testing.T) {
	// A kept slot proves only that giving it back failed, not that work is
	// in it: its lock may have been busy.
	kept := diffNothing(batch.TaskResult{ID: "locked", Slot: 1,
		Error: "slot 1 kept: slot 1 is locked by another process"})
	for _, want := range []string{"task locked committed nothing", "slot 1 was kept", "inspect it", "hrn ws list"} {
		if !strings.Contains(kept, want) {
			t.Errorf("a kept slot: %q lacks %q", kept, want)
		}
	}
	for _, claim := range []string{"changed nothing", "its work", "holds"} {
		if strings.Contains(kept, claim) {
			t.Errorf("a kept slot says only that it was kept: %q claims %q", kept, claim)
		}
	}
	quiet := diffNothing(batch.TaskResult{ID: "quiet", Slot: 1})
	if quiet != "task quiet committed nothing" {
		t.Errorf("a quiet slot task: %q", quiet)
	}
	if fresh := diffNothing(batch.TaskResult{ID: "old", Worktree: "wt"}); fresh != "task old changed nothing" {
		t.Errorf("a fresh task: %q", fresh)
	}
}
