package batch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// stub backends: "wb-ok" writes out.txt into the workspace, "wb-noop" changes
// nothing, "wb-bad" fails, "wb-block" waits for cancellation.
type stub struct {
	name string
	run  func(ctx context.Context, task agent.Task) agent.Result
}

func (s stub) Name() string            { return s.name }
func (s stub) Detect() (string, error) { return "stub", nil }
func (s stub) Run(ctx context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
	emit(agent.Event{Kind: agent.EventToolCall, Tool: "write_file", Text: "out.txt"})
	return s.run(ctx, task)
}

var blockStarted = make(chan struct{}, 8)

func init() {
	agent.Register(stub{"wb-ok", func(_ context.Context, task agent.Task) agent.Result {
		_ = os.WriteFile(filepath.Join(task.Dir, "out.txt"), []byte(typed(task)+"\n"), 0o644)
		return agent.Result{Backend: "wb-ok", Status: agent.StatusOK, Final: "wrote out.txt", Turns: 2}
	}})
	agent.Register(leanStub{stub{"wb-lean", func(_ context.Context, task agent.Task) agent.Result {
		return agent.Result{Backend: "wb-lean", Status: agent.StatusOK, Final: "nothing", Turns: 1}
	}}})
	agent.Register(stub{"wb-file", func(_ context.Context, task agent.Task) agent.Result {
		name := strings.Fields(typed(task))[0] + ".txt"
		_ = os.WriteFile(filepath.Join(task.Dir, name), []byte(typed(task)+"\n"), 0o644)
		return agent.Result{Backend: "wb-file", Status: agent.StatusOK, Final: "wrote " + name, Turns: 1}
	}})
	agent.Register(stub{"wb-noop", func(context.Context, agent.Task) agent.Result {
		return agent.Result{Backend: "wb-noop", Status: agent.StatusOK, Final: "nothing to do", Turns: 1}
	}})
	agent.Register(stub{"wb-bad", func(context.Context, agent.Task) agent.Result {
		return agent.Result{Backend: "wb-bad", Status: agent.StatusError, Error: "model exploded", Turns: 1}
	}})
	agent.Register(stub{"wb-bad-writes", func(_ context.Context, task agent.Task) agent.Result {
		_ = os.WriteFile(filepath.Join(task.Dir, "leftover.txt"), []byte("half-done"), 0o644)
		return agent.Result{Backend: "wb-bad-writes", Status: agent.StatusError, Error: "model exploded", Turns: 1}
	}})
	agent.Register(stub{"wb-timeout", func(_ context.Context, task agent.Task) agent.Result {
		_ = os.WriteFile(filepath.Join(task.Dir, "half.txt"), []byte("interrupted"), 0o644)
		return agent.Result{Backend: "wb-timeout", Status: agent.StatusTimeout, Error: "deadline passed", Turns: 1}
	}})
	agent.Register(stub{"wb-block", func(ctx context.Context, _ agent.Task) agent.Result {
		blockStarted <- struct{}{}
		<-ctx.Done()
		return agent.Result{Backend: "wb-block", Status: agent.StatusCanceled, Error: ctx.Err().Error()}
	}})
	agent.Register(stub{"wb-peek", func(context.Context, agent.Task) agent.Result {
		peek()
		return agent.Result{Backend: "wb-peek", Status: agent.StatusOK, Final: "looked", Turns: 1}
	}})
}

// peek is what wb-peek does while it "works"; a test sets it.
var peek = func() {}

// typed is the task's prompt without the commit block agent.Run puts first.
func typed(task agent.Task) string {
	rest, ok := strings.CutPrefix(task.Prompt, "# How to commit\n\n")
	if !ok {
		return task.Prompt
	}
	_, after, _ := strings.Cut(rest, "\n\n")
	return after
}

func TestABatchTasksAgentIsToldHowToCommit(t *testing.T) {
	var seen string
	agent.Register(stub{"wb-seen", func(_ context.Context, task agent.Task) agent.Result {
		seen = task.Prompt
		return agent.Result{Backend: "wb-seen", Status: agent.StatusOK, Final: "looked", Turns: 1}
	}})
	dir := initRepo(t)
	if _, err := Run(context.Background(), Spec{Dir: dir, Backend: "wb-seen", Tasks: []Task{{ID: "one", Prompt: "do it"}}}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(seen, "# How to commit\n\nReviews are text, commits are changes.") || !strings.HasSuffix(seen, "\n\ndo it") {
		t.Errorf("the batch task's agent got %q, want the commit block before the prompt", seen)
	}
	if strings.Contains(seen, "fix round") {
		t.Errorf("a batch task is not a fix round: %q", seen)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// initRepo makes a repository with one commit and points HRN_HOME at a temp dir.
func initRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("HRN_HOME", t.TempDir())
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "README.md")
	git(t, dir, "commit", "-q", "-m", "init")
	return dir
}

// canon resolves symlinks and Windows 8.3 short names, which git expands.
func canon(t *testing.T, p string) string {
	t.Helper()
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func run(t *testing.T, spec Spec) (Result, []Event) {
	t.Helper()
	var mu sync.Mutex
	var events []Event
	res, err := Run(context.Background(), spec, func(e Event) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	return res, events
}

// firstRun loads the record of a task's first attempt. A task with none failed
// before it ever reached a backend, and its own error says why.
func firstRun(t *testing.T, tr TaskResult) runlog.Record {
	t.Helper()
	if len(tr.Attempts) == 0 {
		t.Fatalf("task %s made no attempt: status=%s error=%s", tr.ID, tr.Status, tr.Error)
	}
	rec, err := runlog.Load(tr.Attempts[0].RunID)
	if err != nil {
		t.Fatalf("task %s: loading run %s: %v", tr.ID, tr.Attempts[0].RunID, err)
	}
	return rec
}

func TestRunPassesGateAndCommits(t *testing.T) {
	dir := initRepo(t)
	base := git(t, dir, "rev-parse", "HEAD")
	res, events := run(t, Spec{
		Dir: dir, Parallel: 2, Worktrees: "fresh", Backend: "wb-ok", Gate: []string{"exit 0"},
		Tasks: []Task{{ID: "one", Prompt: "first task"}, {Prompt: "second task"}},
	})
	if res.ID == "" || canon(t, res.Dir) != canon(t, dir) || res.Base != base || len(res.Tasks) != 2 {
		t.Fatalf("result = %+v", res)
	}
	for i, want := range []string{"one", "t2"} {
		tr := res.Tasks[i]
		if tr.ID != want || tr.Status != StatusPass || tr.Branch != "hrn/"+want || len(tr.Attempts) != 1 {
			t.Fatalf("task %d = %+v", i, tr)
		}
		a := tr.Attempts[0]
		if a.Backend != "wb-ok" || a.RunID == "" || a.Result.Status != agent.StatusOK || len(a.Gates) != 1 || a.Gates[0].ExitCode != 0 {
			t.Errorf("attempt = %+v", a)
		}
		if data, err := os.ReadFile(filepath.Join(tr.Worktree, "out.txt")); err != nil || !strings.Contains(string(data), "task") {
			t.Errorf("worktree output: %q err=%v", data, err)
		}
		if tr.Commit == "" || git(t, dir, "rev-parse", tr.Branch) != tr.Commit {
			t.Errorf("branch %s should point at the task commit %q", tr.Branch, tr.Commit)
		}
		if files := git(t, dir, "show", "--stat", "--format=", tr.Commit); !strings.Contains(files, "out.txt") {
			t.Errorf("commit should contain out.txt: %s", files)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); err == nil {
		t.Error("main worktree must stay untouched")
	}
	saved, err := LoadResult(res.ID)
	if err != nil || saved.ID != res.ID || len(saved.Tasks) != 2 || saved.Tasks[0].Status != StatusPass {
		t.Errorf("record not saved: %+v err=%v", saved, err)
	}
	var kinds []string
	for _, e := range events {
		if e.Task == "one" {
			kinds = append(kinds, e.Kind)
		}
	}
	if got := strings.Join(kinds, ","); got != "start,tool,run,gate,done" {
		t.Errorf("events for task one = %s", got)
	}
}

func TestBatchUsesRoutingAndKeepsNamedModel(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmtBool(explicit), func(t *testing.T) {
			dir := initRepo(t)
			name := agent.DefaultBackendName()
			old, _ := agent.Get(name)
			agent.Register(stub{name, func(context.Context, agent.Task) agent.Result {
				return agent.Result{Backend: name, Status: agent.StatusOK}
			}})
			defer agent.Register(old)
			if err := os.WriteFile(filepath.Join(runlog.Home(), "route.json"), []byte(`{"implement":{"backend":"wb-noop","model":"routed","effort":"medium","eval":"e1","set":"2026-10-12"}}`), 0600); err != nil {
				t.Fatal(err)
			}
			task := Task{ID: "route", Prompt: "Implement a widget"}
			if explicit {
				task.Model = "mine"
			}
			res, _ := run(t, Spec{Dir: dir, Worktrees: "fresh", Options: agent.Options{NoSwitch: true}, Tasks: []Task{task}})
			rec := firstRun(t, res.Tasks[0])
			if explicit {
				if rec.Task.Model != "mine" || rec.Task.Effort != "" || rec.Backend != name || rec.RoutedBy != nil {
					t.Fatalf("substituted: %+v", rec)
				}
			} else if rec.Task.Model != "routed" || rec.Task.Effort != "medium" || rec.Backend != "wb-noop" || rec.RoutedBy == nil {
				t.Fatalf("route: %+v", rec)
			}
		})
	}
}

func TestBatchInheritedChoicesAreNotRouted(t *testing.T) {
	for _, choice := range []string{"backend", "model", "effort"} {
		t.Run(choice, func(t *testing.T) {
			dir := initRepo(t)
			name := agent.DefaultBackendName()
			old, err := agent.Get(name)
			if err != nil {
				t.Fatal(err)
			}
			agent.Register(stub{name, func(context.Context, agent.Task) agent.Result {
				return agent.Result{Backend: name, Status: agent.StatusOK}
			}})
			defer agent.Register(old)
			if err := os.WriteFile(filepath.Join(runlog.Home(), "route.json"), []byte(`{"implement":{"backend":"wb-noop","model":"routed","effort":"medium"}}`), 0600); err != nil {
				t.Fatal(err)
			}
			spec := Spec{Dir: dir, Worktrees: "fresh", Options: agent.Options{NoSwitch: true},
				Tasks: []Task{{ID: "inherited", Prompt: "Implement a widget"}}}
			wantBackend := name
			switch choice {
			case "backend":
				spec.Backend, wantBackend = "wb-ok", "wb-ok"
			case "model":
				spec.Model = "mine"
			case "effort":
				spec.Effort = "high"
			}
			res, _ := run(t, spec)
			rec := firstRun(t, res.Tasks[0])
			if res.Tasks[0].Status != StatusPass || rec.Backend != wantBackend || rec.Task.Model != spec.Model || rec.Task.Effort != spec.Effort || rec.RoutedBy != nil {
				t.Fatalf("inherited choice was routed: %+v", rec)
			}
			if err := Remove(res); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func fmtBool(v bool) string {
	if v {
		return "explicit"
	}
	return "routed"
}

func TestFailuresRetryOnNextBackend(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{
		Dir: dir, Worktrees: "fresh", Gate: []string{"exit 0"},
		Tasks: []Task{
			{ID: "recovers", Prompt: "p", Backend: "wb-bad", Retry: []string{"wb-ok"}},
			{ID: "gate-fails", Prompt: "p", Backend: "wb-ok", Gate: []string{"exit 0", "exit 3", "exit 0"}, Retry: []string{"wb-ok"}},
		},
	})
	rec := res.Tasks[0]
	if rec.Status != StatusPass || len(rec.Attempts) != 2 || rec.Attempts[0].Backend != "wb-bad" || rec.Attempts[0].Result.Status != agent.StatusError || rec.Attempts[1].Backend != "wb-ok" {
		t.Fatalf("recovers = %+v", rec)
	}
	if rec.Commit == "" || git(t, dir, "rev-parse", "hrn/recovers") != rec.Commit {
		t.Errorf("retried task should end with its own branch and commit: %+v", rec)
	}

	gf := res.Tasks[1]
	if gf.Status != StatusFail || len(gf.Attempts) != 2 {
		t.Fatalf("gate-fails = %+v", gf)
	}
	for _, a := range gf.Attempts {
		if len(a.Gates) != 2 || a.Gates[0].ExitCode != 0 || a.Gates[1].ExitCode != 3 {
			t.Errorf("gates should stop at the first failure: %+v", a.Gates)
		}
	}
	if _, err := os.Stat(filepath.Join(gf.Worktree, "out.txt")); err != nil {
		t.Errorf("failed task keeps its last worktree for inspection: %v", err)
	}
	if branches := git(t, dir, "branch", "--list", "hrn/*"); strings.Count(branches, "hrn/") != 2 {
		t.Errorf("exactly one branch per task expected:\n%s", branches)
	}
}

func TestNoChangesMeansNoCommit(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-noop", Tasks: []Task{{Prompt: "look only"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusPass || tr.Commit != "" || git(t, dir, "rev-parse", tr.Branch) != res.Base {
		t.Fatalf("task = %+v", tr)
	}
}

func TestCancelStopsEverything(t *testing.T) {
	dir := initRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-blockStarted
		cancel()
	}()
	res, err := Run(ctx, Spec{Dir: dir, Parallel: 1, Backend: "wb-block", Retry: []string{"wb-ok"},
		Tasks: []Task{{Prompt: "a"}, {Prompt: "b"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Tasks[0].Status != StatusCanceled || len(res.Tasks[0].Attempts) != 1 {
		t.Errorf("running task should be canceled without retry: %+v", res.Tasks[0])
	}
	if res.Tasks[1].Status != StatusCanceled || len(res.Tasks[1].Attempts) != 0 {
		t.Errorf("queued task should be canceled untouched: %+v", res.Tasks[1])
	}
}

func TestRecordIsSavedBeforeTasksRun(t *testing.T) {
	dir := initRepo(t)
	batches := filepath.Join(os.Getenv("HRN_HOME"), "batches")
	ctx, cancel := context.WithCancel(context.Background())
	var early Result
	var earlyErr error
	go func() {
		<-blockStarted
		entries, err := os.ReadDir(batches)
		if err != nil || len(entries) != 1 {
			earlyErr = err
		} else {
			var data []byte
			data, earlyErr = os.ReadFile(filepath.Join(batches, entries[0].Name()))
			_ = json.Unmarshal(data, &early)
		}
		cancel()
	}()
	res, err := Run(ctx, Spec{Dir: dir, Backend: "wb-block", Tasks: []Task{{ID: "slow", Prompt: "a"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if earlyErr != nil || early.ID != res.ID || len(early.Tasks) != 1 || early.Tasks[0].Status != StatusRunning || early.Tasks[0].Branch != "hrn/slow" {
		t.Fatalf("record while running = %+v err=%v", early, earlyErr)
	}
	if saved, err := LoadResult(res.ID); err != nil || saved.Tasks[0].Status != StatusCanceled {
		t.Errorf("final record = %+v err=%v", saved, err)
	}
}

func TestRemoveDeletesWorktreesAndBranches(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{Dir: dir, Worktrees: "fresh", Backend: "wb-ok", Tasks: []Task{{Prompt: "a"}, {Prompt: "b"}}})
	if err := Remove(res); err != nil {
		t.Fatal(err)
	}
	for _, tr := range res.Tasks {
		if _, err := os.Stat(tr.Worktree); err == nil {
			t.Errorf("worktree %s still exists", tr.Worktree)
		}
	}
	if branches := git(t, dir, "branch", "--list", "hrn/*"); branches != "" {
		t.Errorf("branches remain: %s", branches)
	}
	if git(t, dir, "worktree", "list") == "" {
		t.Error("main worktree must survive")
	}
}

func TestRemoveTasksDeletesOnlyNamedSquashMergedTask(t *testing.T) {
	dir := initRepo(t)
	res := mergeableFresh(t, dir, Task{ID: "alpha", Prompt: "alpha"}, Task{ID: "beta", Prompt: "beta"})
	merged, err := Merge(context.Background(), res, []string{"alpha"}, true)
	if err != nil || len(merged) != 1 || merged[0].Commit == "" {
		t.Fatalf("merge = %+v, %v", merged, err)
	}
	if _, err := RemoveTasks(res, []string{"alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(res.Tasks[0].Worktree); !os.IsNotExist(err) {
		t.Errorf("merged worktree still exists: %v", err)
	}
	if branches := git(t, dir, "branch", "--list", res.Tasks[0].Branch); branches != "" {
		t.Errorf("squash-merged branch remains: %s", branches)
	}
	if _, err := os.Stat(res.Tasks[1].Worktree); err != nil {
		t.Errorf("unmerged worktree was removed: %v", err)
	}
	if branches := git(t, dir, "branch", "--list", res.Tasks[1].Branch); branches == "" {
		t.Error("unmerged branch was removed")
	}
}

func TestRemoveTasksKeepsDirtyWorktreeAndReportsIt(t *testing.T) {
	dir := initRepo(t)
	res := mergeableFresh(t, dir, Task{ID: "alpha", Prompt: "alpha"})
	if _, err := Merge(context.Background(), res, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(res.Tasks[0].Worktree, "notes.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := RemoveTasks(res, []string{"alpha"})
	if err == nil || !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("dirty worktree error = %v", err)
	}
	if _, err := os.Stat(res.Tasks[0].Worktree); err != nil {
		t.Errorf("dirty worktree was removed: %v", err)
	}
	if branches := git(t, dir, "branch", "--list", res.Tasks[0].Branch); branches == "" {
		t.Error("dirty task branch was removed")
	}
}

func TestRemoveTasksKeepsUnownedWorktree(t *testing.T) {
	dir := initRepo(t)
	res := mergeableFresh(t, dir, Task{ID: "alpha", Prompt: "alpha"})
	res.Tasks[0].BranchMade = false
	if _, err := RemoveTasks(res, []string{"alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(res.Tasks[0].Worktree); err != nil {
		t.Errorf("another batch's worktree was removed: %v", err)
	}
}

func TestRemoveTasksReturnsOnlyIDsWithRemovedWork(t *testing.T) {
	dir := initRepo(t)
	res := mergeableFresh(t, dir, Task{ID: "alpha", Prompt: "alpha"}, Task{ID: "beta", Prompt: "beta"}, Task{ID: "gamma", Prompt: "gamma"})
	res.Tasks[2].BranchMade = false                        // This worktree and branch belong to another batch.
	res.Tasks = append(res.Tasks, TaskResult{ID: "delta"}) // No worktree or branch was made.
	if err := os.WriteFile(filepath.Join(res.Tasks[1].Worktree, "notes.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveTasks(res, []string{"alpha", "beta", "gamma", "delta"})
	if len(removed) != 1 || removed[0] != "alpha" || err == nil ||
		!strings.Contains(err.Error(), "beta") || strings.Contains(err.Error(), "gamma") || strings.Contains(err.Error(), "delta") {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	if _, err := os.Stat(res.Tasks[0].Worktree); !os.IsNotExist(err) {
		t.Errorf("alpha worktree remains: %v", err)
	}
	for _, tr := range res.Tasks[1:3] {
		if _, err := os.Stat(tr.Worktree); err != nil {
			t.Errorf("%s worktree is gone: %v", tr.ID, err)
		}
	}
}

func TestRemoveTasksCountsNothingWhenTheProbeAfterRemovalFails(t *testing.T) {
	dir := initRepo(t)
	res := mergeableFresh(t, dir, Task{ID: "alpha", Prompt: "alpha"})
	if _, err := Merge(context.Background(), res, nil, false); err != nil {
		t.Fatal(err)
	}
	slot := filepath.Join(dir, ".hrn", "slots", "1")
	git(t, dir, "worktree", "remove", res.Tasks[0].Worktree)
	if err := os.MkdirAll(slot, 0o755); err != nil {
		t.Fatal(err)
	}
	res.Tasks[0].Slot, res.Tasks[0].Worktree = 1, slot
	real := branchProbe
	t.Cleanup(func() { branchProbe = real })
	calls := 0
	branchProbe = func(ctx context.Context, dir, ref string) (string, error) {
		calls++
		if calls == 1 {
			return real(ctx, dir, ref)
		}
		return "", errors.New("git timed out")
	}
	removed, err := RemoveTasks(res, []string{"alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v; a probe that failed after the removal cannot prove the branch went", removed)
	}
	if calls != 2 {
		t.Errorf("probe ran %d times, want 2", calls)
	}
}

func TestRemoveTasksKeepsSlotAndDeletesItsBranch(t *testing.T) {
	dir := initRepo(t)
	res := mergeableFresh(t, dir, Task{ID: "alpha", Prompt: "alpha"})
	if _, err := Merge(context.Background(), res, nil, false); err != nil {
		t.Fatal(err)
	}
	slot := filepath.Join(dir, ".hrn", "slots", "1")
	git(t, dir, "worktree", "remove", res.Tasks[0].Worktree)
	if err := os.MkdirAll(slot, 0o755); err != nil {
		t.Fatal(err)
	}
	res.Tasks[0].Slot, res.Tasks[0].Worktree = 1, slot
	if _, err := RemoveTasks(res, []string{"alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(slot); err != nil {
		t.Errorf("slot was removed: %v", err)
	}
	if branches := git(t, dir, "branch", "--list", res.Tasks[0].Branch); branches != "" {
		t.Errorf("slot task branch remains: %s", branches)
	}
}

func TestRemoveTasksRejectsUnknownID(t *testing.T) {
	_, err := RemoveTasks(Result{ID: "batch", Tasks: []TaskResult{{ID: "alpha"}}}, []string{"missing"})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("unknown task error = %v", err)
	}
}

// deregister leaves tr's worktree the way a file lock on Windows can: git no longer lists it,
// its directory is still there (empty, or holding what remains), and its branch still exists.
func deregister(t *testing.T, root string, tr TaskResult, leftover string) {
	t.Helper()
	if err := os.RemoveAll(tr.Worktree); err != nil {
		t.Fatal(err)
	}
	git(t, root, "worktree", "prune")
	if err := os.MkdirAll(tr.Worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if leftover != "" {
		if err := os.WriteFile(filepath.Join(tr.Worktree, leftover), []byte("left behind\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if list := git(t, root, "worktree", "list"); strings.Contains(filepath.ToSlash(list), filepath.Base(tr.Worktree)) {
		t.Fatalf("setup: %s is still registered:\n%s", tr.Worktree, list)
	}
}

func TestRemoveFinishesDeregisteredWorktree(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{Dir: dir, Worktrees: "fresh", Backend: "wb-ok", Tasks: []Task{{ID: "gone", Prompt: "a"}, {ID: "kept", Prompt: "b"}}})
	deregister(t, dir, res.Tasks[0], "")
	// A modified file in the repository itself must not count against the leftover directory:
	// it has no .git of its own, so a status inside it would describe the repository.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Remove(res); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	for _, tr := range res.Tasks {
		if _, err := os.Stat(tr.Worktree); err == nil {
			t.Errorf("%s still exists", tr.Worktree)
		}
	}
	if branches := git(t, dir, "branch", "--list", "hrn/*"); branches != "" {
		t.Errorf("branches remain: %s", branches)
	}
}

func TestRemoveKeepsAndReportsANonEmptyLeftover(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{Dir: dir, Worktrees: "fresh", Backend: "wb-ok", Tasks: []Task{{ID: "gone", Prompt: "a"}, {ID: "kept", Prompt: "b"}}})
	tr := res.Tasks[0]
	deregister(t, dir, tr, "notes.txt")
	err := Remove(res)
	if err == nil || !strings.Contains(err.Error(), "gone") || !strings.Contains(err.Error(), "not a git worktree") {
		t.Fatalf("Remove must report the leftover it could not remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tr.Worktree, "notes.txt")); err != nil {
		t.Errorf("files no branch holds must stay: %v", err)
	}
	// Everything else is still cleaned: the leftover's branch and the other task entirely.
	if branches := git(t, dir, "branch", "--list", "hrn/*"); branches != "" {
		t.Errorf("branches remain: %s", branches)
	}
	if _, err := os.Stat(res.Tasks[1].Worktree); err == nil {
		t.Errorf("the other task's worktree %s still exists", res.Tasks[1].Worktree)
	}
}

func TestCommitFailureIsAnErrorAndRemoveRefuses(t *testing.T) {
	dir := initRepo(t)
	hook := filepath.Join(dir, ".git", "hooks", "pre-commit")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, _ := run(t, Spec{Dir: dir, Worktrees: "fresh", Backend: "wb-ok", Retry: []string{"wb-ok"}, Tasks: []Task{{ID: "hooked", Prompt: "a"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusError || !strings.Contains(tr.Error, "uncommitted") || len(tr.Attempts) != 1 || tr.Commit != "" {
		t.Fatalf("task = %+v", tr)
	}
	if _, err := os.Stat(filepath.Join(tr.Worktree, "out.txt")); err != nil {
		t.Fatalf("the agent's work must stay in the worktree: %v", err)
	}
	err := Remove(res)
	if err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("Remove must refuse a dirty worktree: %v", err)
	}
	if _, err := os.Stat(tr.Worktree); err != nil {
		t.Error("dirty worktree was removed")
	}
}

func TestBranchCollisionIsAnError(t *testing.T) {
	dir := initRepo(t)
	git(t, dir, "branch", "hrn/taken")
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-ok", Retry: []string{"wb-ok"}, Tasks: []Task{{ID: "taken", Prompt: "a"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusError || tr.Error == "" || len(tr.Attempts) != 0 {
		t.Fatalf("existing branch must fail fast without attempts: %+v", tr)
	}
}

// A fresh task that finds hrn/<id> or .hrn/worktrees/<id> already there
// (another batch's) fails at once, though its record names that worktree (Run
// sets it before the task starts): hrn batch rm of its batch must leave them,
// and they must not keep its record (#162).
func TestRemoveLeavesAnotherFreshBatchsWorktreeThatFailedATask(t *testing.T) {
	for _, tc := range []struct {
		name  string
		taken func(t *testing.T, dir string) (worktree string) // what the other batch left
	}{
		{"branch and worktree", func(t *testing.T, dir string) string {
			a, _ := run(t, Spec{Dir: dir, Worktrees: "fresh", Backend: "wb-ok", Tasks: []Task{{ID: "x", Prompt: "a"}}})
			if tr := a.Tasks[0]; tr.Status != StatusPass || !tr.BranchMade {
				t.Errorf("the other batch's task = %+v, want a pass whose record says it made its worktree", tr)
			}
			return a.Tasks[0].Worktree
		}},
		{"worktree only", func(t *testing.T, dir string) string {
			wt := filepath.Join(dir, ".hrn", "worktrees", "x")
			git(t, dir, "worktree", "add", "-q", "-b", "other", wt)
			writeFile(t, filepath.Join(wt, "out.txt"), "theirs\n")
			git(t, wt, "add", "out.txt")
			git(t, wt, "commit", "-q", "-m", "theirs")
			return wt
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := initRepo(t)
			wt := tc.taken(t, dir)
			branches := git(t, dir, "branch", "--list", "hrn/*", "other")
			b, _ := run(t, Spec{Dir: dir, Worktrees: "fresh", Backend: "wb-ok", Tasks: []Task{{ID: "x", Prompt: "b"}}})
			tr := b.Tasks[0]
			if tr.Status != StatusError || !strings.Contains(tr.Error, "already exists") || len(tr.Attempts) != 0 || tr.BranchMade {
				t.Fatalf("task = %+v, want a fast failure that made nothing", tr)
			}
			if WorktreesRemain(b) {
				t.Error("another batch's worktree must not keep this record")
			}
			if err := Remove(b); err != nil {
				t.Fatal(err)
			}
			if !exists(filepath.Join(wt, "out.txt")) {
				t.Error("another batch's worktree must stay")
			}
			if got := git(t, dir, "branch", "--list", "hrn/*", "other"); got != branches {
				t.Errorf("branches = %q, want %q left as they were", got, branches)
			}
		})
	}
}

// resave saves res as it is and reads it back as hrn batch rm and prune do.
func resave(t *testing.T, res Result) Result {
	t.Helper()
	if err := save(res); err != nil {
		t.Fatal(err)
	}
	saved, err := LoadResult(res.ID)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

// legacy turns res into a record as hrn wrote it before Ownership: no
// ownership, and no branch_made on a fresh task.
func legacy(t *testing.T, res Result) Result {
	t.Helper()
	res.Ownership = 0
	for i := range res.Tasks {
		if res.Tasks[i].Slot == 0 && res.Tasks[i].Worktree != "" {
			res.Tasks[i].BranchMade = false
		}
	}
	saved := resave(t, res)
	if saved.Ownership != 0 {
		t.Fatalf("setup: ownership = %d, want a record without it", saved.Ownership)
	}
	return saved
}

// Records saved before branch_made was set for fresh tasks lack it: a fresh
// task in one that did not fail on a taken worktree or branch made them, and
// hrn batch rm still removes them.
func TestRemoveStillCleansAFreshTaskFromARecordWithoutBranchMade(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{Dir: dir, Worktrees: "fresh", Backend: "wb-ok", Tasks: []Task{{ID: "old", Prompt: "a"}}})
	res = legacy(t, res)
	if !WorktreesRemain(res) {
		t.Error("the task's worktree must keep the record")
	}
	if err := Remove(res); err != nil {
		t.Fatal(err)
	}
	if exists(res.Tasks[0].Worktree) {
		t.Error("the worktree must go")
	}
	if b := git(t, dir, "branch", "--list", "hrn/old"); b != "" {
		t.Errorf("branch remains: %s", b)
	}
}

// A fresh retry removes the task's worktree and makes it again: the record
// stops claiming it before it goes, and claims it again once it is back.
func TestAFreshRetrySavesItNoLongerOwnsItsWorktreeBeforeRemovingIt(t *testing.T) {
	dir := initRepo(t)
	type seen struct{ made, there bool }
	var saves []seen
	failSave = func(res Result) error {
		if tr := res.Tasks[0]; tr.Status == StatusRunning {
			saves = append(saves, seen{tr.BranchMade, exists(tr.Worktree)})
		}
		return nil
	}
	t.Cleanup(func() { failSave = nil })
	res, _ := run(t, Spec{Dir: dir, Worktrees: "fresh", Backend: "wb-bad", Retry: []string{"wb-ok"}, Tasks: []Task{{ID: "again", Prompt: "p"}}})
	if tr := res.Tasks[0]; tr.Status != StatusPass || len(tr.Attempts) != 2 || !tr.BranchMade {
		t.Fatalf("task = %+v, want a pass on the retry that owns its worktree", tr)
	}
	// Made, then given up while the worktree is still there, then made again.
	step := 0
	for _, s := range saves {
		switch {
		case step == 0 && s.made && s.there, step == 2 && s.made && s.there:
			step++
		case step == 1 && !s.made:
			if !s.there {
				t.Fatalf("the claim was dropped only after the worktree went: %+v", saves)
			}
			step++
		}
	}
	if step != 3 {
		t.Errorf("running saves (branch_made, worktree there) = %+v, want made, given up, made again", saves)
	}
}

// A fresh retry gives up its claim on the worktree before removing it, and
// hrn can stop before it claims the new one (or before a first task finds its
// worktree taken): the record then says running, no error, no branch_made.
// Another batch may hold .hrn/worktrees/<id> and hrn/<id> by then, and hrn
// batch rm of this record must leave them.
func TestRemoveLeavesAWorktreeARunningFreshTaskNoLongerClaims(t *testing.T) {
	for name, attempts := range map[string]int{"before the first attempt": 0, "between attempts": 1} {
		t.Run(name, func(t *testing.T) {
			dir := initRepo(t)
			other, _ := run(t, Spec{Dir: dir, Worktrees: "fresh", Backend: "wb-ok", Tasks: []Task{{ID: "x", Prompt: "theirs"}}})
			wt := other.Tasks[0].Worktree
			mine, _ := run(t, Spec{Dir: dir, Worktrees: "fresh", Backend: "wb-ok", Tasks: []Task{{ID: "y", Prompt: "mine"}}})
			if mine.Ownership != OwnershipClaim {
				t.Fatalf("ownership = %d, want %d on every new record", mine.Ownership, OwnershipClaim)
			}
			// Where this task stood when hrn stopped: named x, like the other one.
			tr := &mine.Tasks[0]
			tr.ID, tr.Branch, tr.Worktree = "x", "hrn/x", wt
			tr.Status, tr.Error, tr.BranchMade, tr.Commit = StatusRunning, "", false, ""
			tr.Attempts = tr.Attempts[:attempts]
			saved := resave(t, mine)
			if WorktreesRemain(saved) {
				t.Error("another batch's worktree must not keep this record")
			}
			if err := Remove(saved); err != nil {
				t.Fatal(err)
			}
			if !exists(filepath.Join(wt, "out.txt")) {
				t.Error("another batch's worktree must stay")
			}
			if b := git(t, dir, "branch", "--list", "hrn/x"); b == "" {
				t.Error("another batch's branch must stay")
			}
		})
	}
}

// A fresh retry that cannot remove its worktree keeps it, and its record
// claims it again: hrn batch rm cleans what only the task made.
func TestAFreshRetryThatCannotRemoveItsWorktreeClaimsItAgain(t *testing.T) {
	dir := initRepo(t)
	failSave = func(res Result) error {
		if tr := res.Tasks[0]; tr.Status == StatusRunning && len(tr.Attempts) == 1 && !tr.BranchMade {
			git(t, dir, "worktree", "lock", tr.Worktree) // git worktree remove --force refuses it
		}
		return nil
	}
	t.Cleanup(func() { failSave = nil })
	res, _ := run(t, Spec{Dir: dir, Worktrees: "fresh", Backend: "wb-bad", Retry: []string{"wb-ok"}, Tasks: []Task{{ID: "locked", Prompt: "p"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusError || len(tr.Attempts) != 1 || !exists(tr.Worktree) {
		t.Fatalf("task = %+v, want an error with the worktree still there", tr)
	}
	saved, err := LoadResult(res.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Tasks[0].BranchMade || !WorktreesRemain(saved) {
		t.Errorf("task = %+v, want the record to claim the worktree it still has", saved.Tasks[0])
	}
}

// A fresh retry whose removal stops partway claims only what is still the
// task's: its branch, when git would not delete it, but not a directory git
// no longer knows, with the branch already gone.
func TestAFreshRetryThatRemovesPartOfItsWorktreeClaimsWhatIsLeft(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cut   func(t *testing.T, dir string, tr TaskResult) // as the claim is withdrawn
		claim bool
	}{
		{"branch left", func(t *testing.T, dir string, tr TaskResult) {
			writeFile(t, filepath.Join(dir, ".git", "refs", "heads", "hrn", tr.ID+".lock"), "") // git branch -D refuses
		}, true},
		{"directory left", func(t *testing.T, dir string, tr TaskResult) {
			git(t, dir, "worktree", "remove", "--force", tr.Worktree)
			writeFile(t, filepath.Join(tr.Worktree, "stray.txt"), "not git's\n")
		}, false},
		// git cannot list worktrees once the branch is gone: no claim from that.
		{"directory left, worktrees unlisted", func(t *testing.T, dir string, tr TaskResult) {
			git(t, dir, "worktree", "remove", "--force", tr.Worktree)
			writeFile(t, filepath.Join(tr.Worktree, "stray.txt"), "not git's\n")
			failWorktreeList = func() error {
				if git(t, dir, "branch", "--list", tr.Branch) == "" {
					return errors.New("cannot list worktrees")
				}
				return nil
			}
		}, false},
		// The removal stops at the listing, so the branch is left, and claimed.
		{"branch left, worktrees unlisted", func(t *testing.T, dir string, tr TaskResult) {
			git(t, dir, "worktree", "remove", "--force", tr.Worktree)
			writeFile(t, filepath.Join(tr.Worktree, "stray.txt"), "not git's\n")
			failWorktreeList = func() error { return errors.New("cannot list worktrees") }
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := initRepo(t)
			broken := false
			failSave = func(res Result) error {
				if tr := res.Tasks[0]; !broken && tr.Status == StatusRunning && len(tr.Attempts) == 1 && !tr.BranchMade {
					broken = true
					tc.cut(t, dir, tr)
				}
				return nil
			}
			t.Cleanup(func() { failSave, failWorktreeList = nil, nil })
			res, _ := run(t, Spec{Dir: dir, Worktrees: "fresh", Backend: "wb-bad", Retry: []string{"wb-ok"}, Tasks: []Task{{ID: "part", Prompt: "p"}}})
			failWorktreeList = nil
			tr := res.Tasks[0]
			if tr.Status != StatusError || len(tr.Attempts) != 1 || exists(filepath.Join(tr.Worktree, ".git")) {
				t.Fatalf("task = %+v, want an error after the worktree went", tr)
			}
			saved, err := LoadResult(res.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got := saved.Tasks[0].BranchMade; got != tc.claim {
				t.Fatalf("branch_made = %v, want %v", got, tc.claim)
			}
			if !tc.claim {
				return
			}
			if err := os.Remove(filepath.Join(dir, ".git", "refs", "heads", "hrn", "part.lock")); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if err := os.RemoveAll(filepath.Join(tr.Worktree, "stray.txt")); err != nil {
				t.Fatal(err)
			}
			if err := Remove(saved); err != nil {
				t.Fatal(err)
			}
			if b := git(t, dir, "branch", "--list", "hrn/part"); b != "" {
				t.Errorf("branch remains: %s", b)
			}
		})
	}
}

// A fresh task whose record cannot claim the worktree it just made removes it
// again: hrn batch rm would leave an unclaimed worktree, and prune could drop
// the record. When the removal fails too, the worktree stays claimed.
func TestAFreshTaskThatCannotSaveItsClaimRemovesItsWorktree(t *testing.T) {
	for name, removable := range map[string]bool{"removed": true, "left": false} {
		t.Run(name, func(t *testing.T) {
			dir := initRepo(t)
			failed := false // fail only the first save that claims the worktree
			failSave = func(res Result) error {
				if tr := res.Tasks[0]; !failed && tr.Status == StatusRunning && tr.BranchMade {
					failed = true
					if !removable {
						git(t, dir, "worktree", "lock", tr.Worktree) // git worktree remove --force refuses it
					}
					return errors.New("disk full")
				}
				return nil
			}
			t.Cleanup(func() { failSave = nil })
			res, _ := run(t, Spec{Dir: dir, Worktrees: "fresh", Backend: "wb-ok", Tasks: []Task{{ID: "claim", Prompt: "p"}}})
			tr := res.Tasks[0]
			if tr.Status != StatusError || len(tr.Attempts) != 0 || !strings.Contains(tr.Error, "disk full") {
				t.Fatalf("task = %+v, want an error naming the failed save before any attempt", tr)
			}
			saved, err := LoadResult(res.ID)
			if err != nil {
				t.Fatal(err)
			}
			b := git(t, dir, "branch", "--list", "hrn/claim")
			if removable {
				if exists(tr.Worktree) || b != "" {
					t.Errorf("worktree there %v, branch %q: want both gone", exists(tr.Worktree), b)
				}
				if saved.Tasks[0].BranchMade {
					t.Error("the record must not claim a worktree that is gone")
				}
				return
			}
			git(t, dir, "worktree", "unlock", tr.Worktree)
			if !strings.Contains(tr.Error, "worktree "+tr.Worktree+" and branch hrn/claim are left") || !exists(tr.Worktree) || b == "" {
				t.Errorf("task = %+v, branch %q: want the worktree and branch left, and said", tr, b)
			}
			if !saved.Tasks[0].BranchMade || !WorktreesRemain(saved) {
				t.Errorf("task = %+v, want the record to claim what is left", saved.Tasks[0])
			}
		})
	}
}

// When that removal stops at a directory git no longer knows, the error names
// the directory alone: the worktree and branch are gone.
func TestAFreshTaskThatCannotSaveItsClaimSaysWhatIsLeft(t *testing.T) {
	dir := initRepo(t)
	failed := false
	failSave = func(res Result) error {
		if tr := res.Tasks[0]; !failed && tr.Status == StatusRunning && tr.BranchMade {
			failed = true
			git(t, dir, "worktree", "remove", "--force", tr.Worktree)
			writeFile(t, filepath.Join(tr.Worktree, "stray.txt"), "not git's\n")
			return errors.New("disk full")
		}
		return nil
	}
	t.Cleanup(func() { failSave = nil })
	res, _ := run(t, Spec{Dir: dir, Worktrees: "fresh", Backend: "wb-ok", Tasks: []Task{{ID: "claim", Prompt: "p"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusError || !strings.Contains(tr.Error, "disk full") {
		t.Fatalf("task = %+v, want an error naming the failed save", tr)
	}
	if !strings.Contains(tr.Error, "directory "+tr.Worktree+" is left") || strings.Contains(tr.Error, "branch") {
		t.Errorf("error = %q, want only the directory said to be left", tr.Error)
	}
	if b := git(t, dir, "branch", "--list", "hrn/claim"); b != "" || tr.BranchMade {
		t.Errorf("branch %q, branch_made %v: want the branch gone and unclaimed", b, tr.BranchMade)
	}
}

// In a record from before Ownership, a fresh task that ran made its worktree,
// whatever its error says: output carried into it can say "already exists".
func TestRemoveCleansALegacyFreshTaskThatRanWhateverItsErrorSays(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{Dir: dir, Worktrees: "fresh", Backend: "wb-ok", Tasks: []Task{{ID: "ran", Prompt: "a"}}})
	res.Tasks[0].Error = "commit: hook: lock file .git/index.lock already exists"
	res = legacy(t, res)
	if !WorktreesRemain(res) {
		t.Error("the task's worktree must keep the record")
	}
	if err := Remove(res); err != nil {
		t.Fatal(err)
	}
	if exists(res.Tasks[0].Worktree) {
		t.Error("the worktree must go")
	}
	if b := git(t, dir, "branch", "--list", "hrn/ran"); b != "" {
		t.Errorf("branch remains: %s", b)
	}
}

// In a record from before Ownership, a fresh task that failed on hrn's own
// check (its worktree or branch was there already) made nothing: they stay.
func TestRemoveLeavesWhatALegacyFreshTaskFoundTaken(t *testing.T) {
	dir := initRepo(t)
	other, _ := run(t, Spec{Dir: dir, Worktrees: "fresh", Backend: "wb-ok", Tasks: []Task{{ID: "x", Prompt: "theirs"}}})
	wt := other.Tasks[0].Worktree
	res, _ := run(t, Spec{Dir: dir, Worktrees: "fresh", Backend: "wb-ok", Tasks: []Task{{ID: "x", Prompt: "mine"}}})
	if tr := res.Tasks[0]; tr.Status != StatusError || len(tr.Attempts) != 0 {
		t.Fatalf("task = %+v, want hrn's collision error", tr)
	}
	res = legacy(t, res)
	if WorktreesRemain(res) {
		t.Error("another batch's worktree must not keep this record")
	}
	if err := Remove(res); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(wt, "out.txt")) {
		t.Error("another batch's worktree must stay")
	}
	if b := git(t, dir, "branch", "--list", "hrn/x"); b == "" {
		t.Error("another batch's branch must stay")
	}
}

func TestDirtyRepositoryIsRefused(t *testing.T) {
	dir := initRepo(t)
	spec := func(allowDirty bool) Spec {
		return Spec{Dir: dir, Worktrees: "fresh", Backend: "wb-ok", AllowDirty: allowDirty, Tasks: []Task{{ID: "t", Prompt: "a"}}}
	}
	// An untracked file is not a reason to stop: .hrn/worktrees is one.
	if err := os.WriteFile(filepath.Join(dir, "scratch.txt"), []byte("notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ := run(t, spec(false))
	if res.Tasks[0].Status != StatusPass {
		t.Fatalf("an untracked file must not stop a batch: %+v", res.Tasks[0])
	}
	if err := Remove(res); err != nil {
		t.Fatal(err)
	}

	// A modified tracked file is, because the agents branch from HEAD.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), spec(false), nil)
	if err == nil || !strings.Contains(err.Error(), "uncommitted changes") || !strings.Contains(err.Error(), "README.md") {
		t.Fatalf("a dirty repository must be refused by name: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".hrn", "worktrees", "t")); statErr == nil {
		t.Error("nothing should be created when the batch refuses to start")
	}

	res, _ = run(t, spec(true))
	if res.Tasks[0].Status != StatusPass {
		t.Fatalf("--allow-dirty must let it run: %+v", res.Tasks[0])
	}
}

func TestAllowDangerousReachesTheTask(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-ok", Tasks: []Task{
		{ID: "spec-wide", Prompt: "a"},
	}, Options: agent.Options{AllowDangerous: true}})
	if rec := firstRun(t, res.Tasks[0]); !rec.Task.AllowDangerous {
		t.Fatalf("the spec-wide switch should reach the task: %+v", rec.Task)
	}

	dir = initRepo(t)
	res, _ = run(t, Spec{Dir: dir, Backend: "wb-ok", Tasks: []Task{
		{ID: "off", Prompt: "a"},
		{ID: "on", Prompt: "a", Options: agent.Options{AllowDangerous: true}},
	}})
	for i, want := range []bool{false, true} {
		if rec := firstRun(t, res.Tasks[i]); rec.Task.AllowDangerous != want {
			t.Errorf("%s: AllowDangerous = %v, want %v", res.Tasks[i].ID, rec.Task.AllowDangerous, want)
		}
	}
}

func TestTaskMCPListsAreResolvedBeforeAnythingRuns(t *testing.T) {
	dir := initRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "servers.json"), []byte(`{"mcpServers":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-ok", Tasks: []Task{
		{ID: "own", Prompt: "a", Options: agent.Options{MCP: []string{"servers.json"}}},
		{ID: "none", Prompt: "a", Options: agent.Options{MCP: []string{}}},
	}})
	own := firstRun(t, res.Tasks[0])
	if len(own.Task.MCP) == 0 || !filepath.IsAbs(own.Task.MCP[len(own.Task.MCP)-1]) ||
		filepath.Base(own.Task.MCP[len(own.Task.MCP)-1]) != "servers.json" {
		t.Errorf("a task's mcp file should reach it resolved against the spec's dir: %v", own.Task.MCP)
	}
	if none := firstRun(t, res.Tasks[1]); len(none.Task.MCP) != 0 {
		t.Errorf("an empty list means no servers, got %v", none.Task.MCP)
	}

	// A typo in one task's list stops the batch before any task runs.
	_, err := Run(context.Background(), Spec{Dir: dir, Backend: "wb-ok", Tasks: []Task{
		{ID: "fine", Prompt: "a"},
		{ID: "typo", Prompt: "a", Options: agent.Options{MCP: []string{"mpc.json"}}},
	}}, nil)
	if err == nil || !strings.Contains(err.Error(), "task typo") || !strings.Contains(err.Error(), "mpc.json") {
		t.Fatalf("a bad task mcp path must fail the batch up front, by task: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".hrn", "worktrees", "fine")); statErr == nil {
		t.Error("no task should have run")
	}
}

func TestSpecValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec Spec
		want string
	}{
		{"unknown backend", Spec{Backend: "nope", Tasks: []Task{{Prompt: "a"}}}, "unknown backend"},
		{"unknown retry", Spec{Backend: "wb-ok", Retry: []string{"nope"}, Tasks: []Task{{Prompt: "a"}}}, "unknown backend"},
		{"unknown task backend", Spec{Tasks: []Task{{Prompt: "a", Backend: "nope"}}}, "task t1"},
		{"unknown task retry", Spec{Tasks: []Task{{Prompt: "a", Backend: "wb-ok", Retry: []string{"nope"}}}}, "task t1"},
		{"blank gate", Spec{Backend: "wb-ok", Gate: []string{"go test", " "}, Tasks: []Task{{Prompt: "a"}}}, "empty gate"},
		{"blank task gate", Spec{Backend: "wb-ok", Tasks: []Task{{Prompt: "a", Gate: []string{""}}}}, "task t1"},
		{"unknown effort", Spec{Backend: "wb-ok", Options: agent.Options{Effort: "turbo"}, Tasks: []Task{{Prompt: "a"}}}, `unknown effort "turbo"`},
		{"unknown task effort", Spec{Backend: "wb-ok", Tasks: []Task{{Prompt: "a", Options: agent.Options{Effort: "HIGH"}}}}, "task t1"},
	} {
		if _, err := Run(context.Background(), tc.spec, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

// A spec that does not say how many agents run at once gets the default: four,
// as the help text and the MCP tool's description say.
func TestASpecWithoutParallelGetsTheDefault(t *testing.T) {
	if DefaultParallel != 4 {
		t.Fatalf("DefaultParallel = %d, but the help and the MCP description say 4", DefaultParallel)
	}
	p := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(p, []byte(`{"tasks":[{"prompt":"a"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err := LoadSpec(p)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Parallel != DefaultParallel {
		t.Errorf("parallel = %d, want the default %d", spec.Parallel, DefaultParallel)
	}
}

func TestLoadSpec(t *testing.T) {
	p := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(p, []byte(`{"parallel":3,"gate":["go test ./..."],"tasks":[{"prompt":"a"},{"id":"b","prompt":"b","mode":"yolo"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err := LoadSpec(p)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Parallel != 3 || len(spec.Tasks) != 2 || spec.Tasks[0].ID != "t1" || spec.Tasks[1].ID != "b" || spec.Tasks[1].Mode != "yolo" {
		t.Fatalf("spec = %+v", spec)
	}
	if spec.Dir != filepath.Dir(p) {
		t.Errorf("dir should default to the spec file's directory: %q", spec.Dir)
	}
	for _, bad := range []string{
		`{"tasks":[]}`,
		`{"tasks":[{"prompt":""}]}`,
		`{"tasks":[{"id":"x","prompt":"a"},{"id":"x","prompt":"b"}]}`,
		`{"tasks":[{"id":"has space","prompt":"a"}]}`,
		`{"tasks":[{"prompt":"a","mode":"nope"}]}`,
		`{"timeout":"5 minutes","tasks":[{"prompt":"a"}]}`,
		`{"tasks":[{"prompt":"a","timeout":"soon"}]}`,
	} {
		_ = os.WriteFile(p, []byte(bad), 0o644)
		if _, err := LoadSpec(p); err == nil {
			t.Errorf("spec %s should be rejected", bad)
		}
	}
	var r Result
	if err := json.Unmarshal([]byte(`{"id":"x","tasks":[{"id":"a","status":"pass","duration_ns":1000}]}`), &r); err != nil || r.Tasks[0].Duration != time.Microsecond {
		t.Errorf("result should round-trip through JSON: %+v err=%v", r, err)
	}
}

func TestNoInstructionsReachesTheTask(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-ok", Tasks: []Task{
		{ID: "spec-off", Prompt: "a"},
		{ID: "task-off", Prompt: "a", Options: agent.Options{NoInstructions: true}},
	}})
	for _, tc := range []struct {
		i    int
		want bool
	}{{0, false}, {1, true}} {
		if rec := firstRun(t, res.Tasks[tc.i]); rec.Task.NoInstructions != tc.want {
			t.Errorf("%s: NoInstructions = %v, want %v", res.Tasks[tc.i].ID, rec.Task.NoInstructions, tc.want)
		}
	}

	dir = initRepo(t)
	res, _ = run(t, Spec{Dir: dir, Backend: "wb-ok", Tasks: []Task{{Prompt: "a"}}, Options: agent.Options{NoInstructions: true}})
	if rec := firstRun(t, res.Tasks[0]); !rec.Task.NoInstructions {
		t.Errorf("the spec-wide switch should reach every task: %+v", rec.Task)
	}
}

// The gate fails the first time and passes the second, but only if the flag
// file it leaves behind survives, which it does only in a kept worktree.
const flagGate = `test -e passed.flag || { touch passed.flag; echo "flag was missing"; exit 3; }`

func eventTexts(events []Event, kind string) string {
	var out []string
	for _, e := range events {
		if e.Kind == kind {
			out = append(out, e.Text)
		}
	}
	return strings.Join(out, "|")
}

func TestGateFailureContinuesInTheSameWorktree(t *testing.T) {
	dir := initRepo(t)
	res, events := run(t, Spec{Dir: dir, Backend: "wb-ok", Retry: []string{"wb-ok"},
		Gate: []string{flagGate}, Tasks: []Task{{ID: "keeps", Prompt: "do the thing"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusPass || len(tr.Attempts) != 2 {
		t.Fatalf("task = %+v", tr)
	}
	a1, a2 := tr.Attempts[0], tr.Attempts[1]
	if a1.Continued || !a2.Continued {
		t.Errorf("continued: attempt 1 = %v, attempt 2 = %v", a1.Continued, a2.Continued)
	}
	if a1.Prompt != "do the thing" {
		t.Errorf("attempt 1 prompt = %q", a1.Prompt)
	}
	for _, want := range []string{"do the thing", "attempt 1 on wb-ok", "exited 3", "flag was missing", "already applied in this workspace"} {
		if !strings.Contains(a2.Prompt, want) {
			t.Errorf("attempt 2 prompt is missing %q:\n%s", want, a2.Prompt)
		}
	}
	// Both attempts' work is on the branch, the second on top of the first.
	if n := git(t, dir, "rev-list", "--count", res.Base+"..hrn/keeps"); n != "2" {
		t.Errorf("commits above base = %s, want 2 (the first attempt's work must survive)", n)
	}
	if subject := git(t, dir, "log", "-1", "--format=%s", "hrn/keeps"); !strings.HasSuffix(subject, "(attempt 2)") {
		t.Errorf("last commit subject = %q", subject)
	}
	if retries := eventTexts(events, "retry"); !strings.Contains(retries, "continuing in worktree") {
		t.Errorf("retry events = %q", retries)
	}
}

func TestRunFailureRetriesInAFreshWorktree(t *testing.T) {
	dir := initRepo(t)
	res, events := run(t, Spec{Dir: dir, Worktrees: "fresh", Backend: "wb-bad-writes", Retry: []string{"wb-ok"},
		Tasks: []Task{{ID: "fresh", Prompt: "do it"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusPass || len(tr.Attempts) != 2 || tr.Attempts[1].Continued {
		t.Fatalf("task = %+v", tr)
	}
	for _, want := range []string{"do it", "ended with error", "model exploded"} {
		if !strings.Contains(tr.Attempts[1].Prompt, want) {
			t.Errorf("attempt 2 prompt is missing %q:\n%s", want, tr.Attempts[1].Prompt)
		}
	}
	if _, err := os.Stat(filepath.Join(tr.Worktree, "leftover.txt")); err == nil {
		t.Error("a fresh attempt must not inherit the failed attempt's changes")
	}
	if n := git(t, dir, "rev-list", "--count", res.Base+"..hrn/fresh"); n != "1" {
		t.Errorf("commits above base = %s, want 1", n)
	}
	if retries := eventTexts(events, "retry"); !strings.Contains(retries, "fresh worktree") {
		t.Errorf("retry events = %q", retries)
	}
}

func TestFenceOutgrowsBackticksInTheOutput(t *testing.T) {
	for _, tc := range []struct{ output, want string }{
		{"plain output", "```"},
		{"see ``` here", "````"},
		{"```` and ```", "`````"},
	} {
		if got := fence(tc.output); got != tc.want {
			t.Errorf("fence(%q) = %q, want %q", tc.output, got, tc.want)
		}
	}
}

func TestSecondRetryKnowsAboutTheFirst(t *testing.T) {
	dir := initRepo(t)
	// Passes only on the third run, and only if the flags survive, which they
	// do only in a worktree that is kept between attempts.
	gate := `test -e two.flag && exit 0; test -e one.flag && { touch two.flag; echo "second miss"; exit 5; }; touch one.flag; echo "first miss"; exit 4`
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-ok", Retry: []string{"wb-ok", "wb-ok"},
		Gate: []string{gate}, Tasks: []Task{{ID: "third", Prompt: "do the thing"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusPass || len(tr.Attempts) != 3 {
		t.Fatalf("task = %+v", tr)
	}
	third := tr.Attempts[2]
	if !third.Continued {
		t.Error("the third attempt should continue in the worktree")
	}
	// The detail is about attempt 2; attempt 1 is there as history.
	for _, want := range []string{"attempt 2 on wb-ok", "exited 5", "second miss", "Earlier attempts", "1 on wb-ok (gate "} {
		if !strings.Contains(third.Prompt, want) {
			t.Errorf("attempt 3 prompt is missing %q:\n%s", want, third.Prompt)
		}
	}
	// Attempt 1 is a one-line summary: its exit code, and no output block of
	// its own (the only fenced block is attempt 2's).
	if !strings.Contains(third.Prompt, "exited 4)") {
		t.Errorf("the history should carry attempt 1's exit code: %s", third.Prompt)
	}
	if n := strings.Count(third.Prompt, "```"); n != 2 {
		t.Errorf("fenced blocks = %d, want one block (two fences)", n)
	}
	if n := git(t, dir, "rev-list", "--count", res.Base+"..hrn/third"); n != "3" {
		t.Errorf("commits above base = %s, want 3", n)
	}
	if subject := git(t, dir, "log", "-1", "--format=%s", "hrn/third"); !strings.HasSuffix(subject, "(attempt 3)") {
		t.Errorf("last commit subject = %q", subject)
	}
}

func TestLimitsReachTheTaskAndAreValidated(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-ok", Tasks: []Task{
		{ID: "from-spec", Prompt: "a"},
		{ID: "own", Prompt: "a", Options: agent.Options{Timeout: agent.Duration(90 * time.Second), MaxCostUSD: 1, TaskBudget: 40000}},
	}, Options: agent.Options{Timeout: agent.Duration(5 * time.Minute), MaxCostUSD: 2.5, TaskBudget: 30000}})
	for _, tc := range []struct {
		i       int
		timeout time.Duration
		cost    float64
		budget  int
	}{
		{0, 5 * time.Minute, 2.5, 30000},
		{1, 90 * time.Second, 1, 40000},
	} {
		rec := firstRun(t, res.Tasks[tc.i])
		if rec.Task.Timeout != agent.Duration(tc.timeout) || rec.Task.MaxCostUSD != tc.cost || rec.Task.TaskBudget != tc.budget {
			t.Errorf("%s: timeout=%s cost=%v budget=%d", res.Tasks[tc.i].ID, rec.Task.Timeout, rec.Task.MaxCostUSD, rec.Task.TaskBudget)
		}
	}

	for _, tc := range []struct {
		spec Spec
		want string
	}{
		{Spec{Backend: "wb-ok", Tasks: []Task{{Prompt: "a"}}, Options: agent.Options{TaskBudget: 100}}, "20000"},
		{Spec{Backend: "wb-ok", Tasks: []Task{{Prompt: "a", Options: agent.Options{TaskBudget: 5}}}}, "task t1"},
	} {
		if _, err := Run(context.Background(), tc.spec, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("err = %v, want it to mention %q", err, tc.want)
		}
	}
}

func TestTimedOutAttemptRetriesFresh(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-timeout", Retry: []string{"wb-ok"},
		Tasks: []Task{{ID: "slow", Prompt: "a"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusPass || len(tr.Attempts) != 2 {
		t.Fatalf("task = %+v", tr)
	}
	if tr.Attempts[1].Continued {
		t.Error("a timed-out attempt leaves work in an unknown state, so the retry must start fresh")
	}
	if !strings.Contains(tr.Attempts[1].Prompt, "ended with timeout") {
		t.Errorf("the retry prompt should say how the attempt ended:\n%s", tr.Attempts[1].Prompt)
	}
	if _, err := os.Stat(filepath.Join(tr.Worktree, "half.txt")); err == nil {
		t.Error("the fresh worktree must not inherit the timed-out attempt's files")
	}
}

func TestNoContextEditsReachesTheTask(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-ok", Tasks: []Task{
		{ID: "default", Prompt: "a"},
		{ID: "opted-out", Prompt: "a", Options: agent.Options{NoContextEdits: true}},
	}})
	for i, want := range []bool{false, true} {
		if rec := firstRun(t, res.Tasks[i]); rec.Task.NoContextEdits != want {
			t.Errorf("%s: NoContextEdits = %v, want %v", res.Tasks[i].ID, rec.Task.NoContextEdits, want)
		}
	}
}

// leanStub is a backend that applies lean, as claude does.
type leanStub struct{ stub }

func (leanStub) AppliesLean() bool { return true }

func TestLeanReachesTheTask(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-ok", Tasks: []Task{
		{ID: "default", Prompt: "a"},
		{ID: "lean", Prompt: "a", Options: agent.Options{Lean: agent.Bool(true)}},
		{ID: "lean-default", Prompt: "a", Backend: "wb-lean"},
		{ID: "not-lean", Prompt: "a", Backend: "wb-lean", Options: agent.Options{Lean: agent.Bool(false)}},
	}})
	// Lean is the default where the backend applies it; elsewhere an unset
	// lean stays unset, so the record does not claim what did not happen.
	for i, want := range []*bool{nil, agent.Bool(true), agent.Bool(true), agent.Bool(false)} {
		if rec := firstRun(t, res.Tasks[i]); !reflect.DeepEqual(rec.Task.Lean, want) {
			t.Errorf("%s: Lean = %v, want %v", res.Tasks[i].ID, rec.Task.Lean, want)
		}
	}
	res, _ = run(t, Spec{Dir: dir, Backend: "wb-lean", Tasks: []Task{{Prompt: "a"}}, Options: agent.Options{Lean: agent.Bool(false)}})
	if rec := firstRun(t, res.Tasks[0]); rec.Task.IsLean() {
		t.Error("a spec-wide lean false must reach every task")
	}
}

func mergeable(t *testing.T, dir string, tasks ...Task) Result {
	t.Helper()
	return mergeableIn(t, Spec{Dir: dir, Backend: "wb-file", Tasks: tasks})
}

// mergeableFresh is mergeable with a fresh worktree per task.
func mergeableFresh(t *testing.T, dir string, tasks ...Task) Result {
	t.Helper()
	return mergeableIn(t, Spec{Dir: dir, Worktrees: "fresh", Backend: "wb-file", Tasks: tasks})
}

func mergeableIn(t *testing.T, spec Spec) Result {
	t.Helper()
	res, _ := run(t, spec)
	for _, tr := range res.Tasks {
		if tr.Status != StatusPass {
			t.Fatalf("setup: task %s = %s (%s)", tr.ID, tr.Status, tr.Error)
		}
	}
	return res
}

func TestMergeBringsTheBranchesIn(t *testing.T) {
	dir := initRepo(t)
	res := mergeable(t, dir, Task{ID: "alpha", Prompt: "alpha"}, Task{ID: "beta", Prompt: "beta"})
	base := git(t, dir, "rev-parse", "HEAD")

	merged, err := Merge(context.Background(), res, nil, false)
	if err != nil || len(merged) != 2 {
		t.Fatalf("merged = %+v err=%v", merged, err)
	}
	for _, m := range merged {
		if m.Commit == "" || m.Skipped != "" || m.Conflict {
			t.Errorf("%s: %+v", m.Task, m)
		}
	}
	if n := git(t, dir, "rev-list", "--count", "--merges", base+"..HEAD"); n != "2" {
		t.Errorf("merge commits = %s, want 2", n)
	}
	for _, name := range []string{"alpha.txt", "beta.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s did not arrive: %v", name, err)
		}
	}
	// Merging again finds nothing left to do rather than making empty commits.
	again, err := Merge(context.Background(), res, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range again {
		if m.Skipped != "already merged" {
			t.Errorf("second run: %s = %+v", m.Task, m)
		}
	}
}

func TestMergeStopsAtTheFirstConflict(t *testing.T) {
	dir := initRepo(t)
	// Both tasks write out.txt with their own prompt, so the second branch
	// cannot merge on top of the first.
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-ok", Tasks: []Task{
		{ID: "one", Prompt: "first"}, {ID: "two", Prompt: "second"}}})
	before := git(t, dir, "rev-parse", "HEAD")

	merged, err := Merge(context.Background(), res, nil, false)
	if err == nil || !strings.Contains(err.Error(), "two") {
		t.Fatalf("a conflict must be reported by task: %v", err)
	}
	if len(merged) != 2 || merged[0].Commit == "" || !merged[1].Conflict {
		t.Fatalf("merged = %+v", merged)
	}
	if status := git(t, dir, "status", "--porcelain"); status != "" {
		t.Errorf("the tree must be clean after a refused merge:\n%s", status)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "out.txt")); strings.TrimSpace(string(data)) != "first" {
		t.Errorf("the first merge should stand: %q", data)
	}
	if n := git(t, dir, "rev-list", "--count", before+"..HEAD"); n == "0" {
		t.Error("the first merge should still be in history")
	}
}

func TestMergeSquashMakesOneCommit(t *testing.T) {
	dir := initRepo(t)
	res := mergeable(t, dir, Task{ID: "alpha", Prompt: "alpha the task"})
	base := git(t, dir, "rev-parse", "HEAD")

	if _, err := Merge(context.Background(), res, []string{"alpha"}, true); err != nil {
		t.Fatal(err)
	}
	if n := git(t, dir, "rev-list", "--count", base+"..HEAD"); n != "1" {
		t.Errorf("commits = %s, want 1", n)
	}
	if n := git(t, dir, "rev-list", "--count", "--merges", base+"..HEAD"); n != "0" {
		t.Errorf("a squash must not leave a merge commit, got %s", n)
	}
	if subject := git(t, dir, "log", "-1", "--format=%s"); subject != "alpha: alpha the task" {
		t.Errorf("subject = %q", subject)
	}
}

func TestMergeSquashTwiceIsNotAConflict(t *testing.T) {
	dir := initRepo(t)
	res := mergeable(t, dir, Task{ID: "alpha", Prompt: "alpha"})
	if _, err := Merge(context.Background(), res, nil, true); err != nil {
		t.Fatal(err)
	}
	merged, err := Merge(context.Background(), res, nil, true)
	if err != nil {
		t.Fatalf("a second squash must not fail: %v", err)
	}
	if len(merged) != 1 || merged[0].Skipped != "already merged" || merged[0].Conflict || merged[0].Commit != "" {
		t.Fatalf("merged = %+v", merged)
	}
	if status := git(t, dir, "status", "--porcelain"); status != "" {
		t.Errorf("the tree must be left clean:\n%s", status)
	}
}

func TestMergeFailureThatIsNotAConflict(t *testing.T) {
	dir := initRepo(t)
	res := mergeable(t, dir, Task{ID: "alpha", Prompt: "alpha"})
	// The squash commit is refused by a hook: a failure, but nothing conflicts.
	hook := filepath.Join(dir, ".git", "hooks", "pre-commit")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	merged, err := Merge(context.Background(), res, nil, true)
	if err == nil {
		t.Fatal("the refused commit should be an error")
	}
	if len(merged) != 1 || merged[0].Conflict || merged[0].Error == "" {
		t.Fatalf("a refused commit is not a conflict: %+v", merged)
	}
	if status := git(t, dir, "status", "--porcelain"); status != "" {
		t.Errorf("the tree must be put back:\n%s", status)
	}
}

func TestDiffSaysWhenTheBranchIsGone(t *testing.T) {
	dir := initRepo(t)
	res := mergeableFresh(t, dir, Task{ID: "alpha", Prompt: "alpha"})
	if err := Remove(res); err != nil {
		t.Fatal(err)
	}
	// The commit object survives until git collects it, so make it unreachable
	// the way a pruned repository would.
	git(t, dir, "reflog", "expire", "--expire=now", "--all")
	git(t, dir, "gc", "--prune=now", "--quiet")
	if _, _, err := Diff(context.Background(), res, "alpha"); err == nil ||
		!strings.Contains(err.Error(), "branch was removed") {
		t.Errorf("err = %v, want a word about the removed branch", err)
	}
}

func TestDiffIsAPatchAndNothingElse(t *testing.T) {
	dir := initRepo(t)
	res := mergeable(t, dir, Task{ID: "alpha", Prompt: "alpha"})
	diff, _, err := Diff(context.Background(), res, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	// git writes warnings to stderr; the patch must start where git apply
	// expects it to and end with the newline git wrote.
	if !strings.HasPrefix(diff, "diff --git ") {
		t.Errorf("diff does not start with a header:\n%q", diff[:min(120, len(diff))])
	}
	if !strings.HasSuffix(diff, "\n") {
		t.Errorf("diff lost its trailing newline: %q", diff[max(0, len(diff)-40):])
	}
}

func TestMergeRefusesAndSkips(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{Dir: dir, Tasks: []Task{
		{ID: "wrote", Prompt: "wrote", Backend: "wb-file"},
		{ID: "quiet", Prompt: "quiet", Backend: "wb-noop"},
		{ID: "broke", Prompt: "broke", Backend: "wb-bad"},
	}})
	if _, err := res.Find("nope"); err == nil || !strings.Contains(err.Error(), "wrote") {
		t.Errorf("an unknown task should list the ids it has: %v", err)
	}

	// A task that changed nothing and a task that failed are both skipped.
	merged, err := Merge(context.Background(), res, []string{"quiet", "broke"}, false)
	if err != nil || len(merged) != 2 {
		t.Fatalf("merged = %+v err=%v", merged, err)
	}
	if merged[0].Skipped != "changed nothing" || !strings.HasPrefix(merged[1].Skipped, "did not pass") {
		t.Errorf("skips = %+v", merged)
	}

	// An unclean repository is refused before anything is merged.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Merge(context.Background(), res, nil, false)
	if err == nil || !strings.Contains(err.Error(), "uncommitted changes") || !strings.Contains(err.Error(), "README.md") {
		t.Fatalf("a dirty repository must be refused by name: %v", err)
	}
}

func TestDiffShowsWhatATaskChanged(t *testing.T) {
	dir := initRepo(t)
	res := mergeable(t, dir, Task{ID: "alpha", Prompt: "alpha"})

	diff, untracked, err := Diff(context.Background(), res, "alpha")
	if err != nil || len(untracked) != 0 {
		t.Fatalf("diff err=%v untracked=%v", err, untracked)
	}
	for _, want := range []string{"alpha.txt", "+alpha"} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff is missing %q:\n%s", want, diff)
		}
	}
	if _, _, err := Diff(context.Background(), res, "nope"); err == nil {
		t.Error("an unknown task must be an error")
	}
}

func TestDiffFallsBackToTheWorktree(t *testing.T) {
	dir := initRepo(t)
	// A pre-commit hook that fails leaves the work in the worktree, staged but
	// never committed, which is the case the fallback is for.
	hook := filepath.Join(dir, ".git", "hooks", "pre-commit")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, _ := run(t, Spec{Dir: dir, Worktrees: "fresh", Backend: "wb-file", Tasks: []Task{{ID: "alpha", Prompt: "alpha"}}})
	if res.Tasks[0].Commit != "" {
		t.Fatalf("setup: the commit should have failed: %+v", res.Tasks[0])
	}
	diff, _, err := Diff(context.Background(), res, "alpha")
	if err != nil || !strings.Contains(diff, "alpha.txt") {
		t.Fatalf("worktree diff = %q err=%v", diff, err)
	}
}

func TestGateTimeoutIsPerTask(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{
		Dir: dir, Parallel: 2, Backend: "wb-ok", Gate: []string{"sleep 5"},
		GateTimeout: agent.Duration(20 * time.Second),
		Tasks: []Task{
			{ID: "quick", Prompt: "task", GateTimeout: agent.Duration(time.Second)},
			{ID: "patient", Prompt: "task", Gate: []string{"exit 0"}},
		},
	})
	quick := res.Tasks[0]
	if quick.Status == StatusPass || len(quick.Attempts) != 1 || len(quick.Attempts[0].Gates) != 1 {
		t.Fatalf("quick = %+v", quick)
	}
	if g := quick.Attempts[0].Gates[0]; g.ExitCode == 0 || !strings.Contains(g.Output, "timed out after 1s") || g.Duration > 3*time.Second {
		t.Errorf("gate = %+v, want it killed after the task's 1s, not the spec's 20s", g)
	}
	if g := quick.Attempts[0].Gates[0]; g.Killed != KilledTimeout || g.Timeout != agent.Duration(time.Second) {
		t.Errorf("gate = %+v, want the record to say the task's 1s limit killed it", g)
	}
	if res.Tasks[1].Status != StatusPass {
		t.Errorf("patient = %+v", res.Tasks[1])
	}
}

const killedAfter2s = `killed after 2s (gate_timeout; set "gate_timeout" in the spec to allow more)`

func TestGateTimeoutIsReportedAsSuchAndTellsTheRetry(t *testing.T) {
	dir := initRepo(t)
	res, events := run(t, Spec{Dir: dir, Backend: "wb-ok", Retry: []string{"wb-ok"},
		Gate: []string{"echo still going; sleep 10"}, GateTimeout: agent.Duration(2 * time.Second),
		Tasks: []Task{{ID: "slow", Prompt: "do the thing"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusFail || len(tr.Attempts) != 2 {
		t.Fatalf("task = %+v", tr)
	}
	// The retry is told the limit killed the gate, not that the code failed,
	// and the history line for the first attempt says the same.
	second := tr.Attempts[1]
	for _, want := range []string{"did not pass its gate. Its changes\nare already applied", "Build on\nthem rather than starting over",
		"The gate command `echo still going; sleep 10` was killed after 2s by gate_timeout.", "still going",
		"The gate did not fail", "time limit of 2s", "a failure it shows from before the kill still needs fixing",
		"only the operator\ncan raise \"gate_timeout\""} {
		if !strings.Contains(second.Prompt, want) {
			t.Errorf("attempt 2 prompt is missing %q:\n%s", want, second.Prompt)
		}
	}
	// The prompt must not tell the agent to set gate_timeout and then say
	// only the operator can: the operator's advice stays out of it.
	if strings.Contains(second.Prompt, "exited -1") || strings.Contains(second.Prompt, "in the spec to allow more") || !second.Continued {
		t.Errorf("attempt 2 = continued %v, prompt:\n%s", second.Continued, second.Prompt)
	}
	if gates := eventTexts(events, "gate"); !strings.Contains(gates, "✗ echo still going; sleep 10 ("+killedAfter2s+")") || strings.Contains(gates, "exit -1") {
		t.Errorf("gate events = %q", gates)
	}
	if got := outcomeOf(tr.Attempts[0]); got != "(gate `echo still going; sleep 10` was killed after 2s by gate_timeout)" {
		t.Errorf("outcomeOf = %q", got)
	}
}

func TestGateThatCannotStartAfterCancelIsCanceled(t *testing.T) {
	// The batch's context ended before the gate started: the shell never ran,
	// which is the same cancellation as a gate killed mid-way, not exited -1.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g := runGate(ctx, t.TempDir(), "echo never", time.Minute)
	if g.ExitCode != -1 || g.Killed != KilledCanceled || g.Timeout != 0 || g.Outcome() != "canceled" || g.Brief() != "was canceled" {
		t.Errorf("gate = %+v, want it recorded as canceled", g)
	}
	if strings.Contains(g.Output, "never") {
		t.Errorf("the gate ran: %q", g.Output)
	}
}

func TestGateCanceledIsRecordedAsSuch(t *testing.T) {
	dir := initRepo(t)
	flag := filepath.ToSlash(filepath.Join(t.TempDir(), "gate.started"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			if _, err := os.Stat(flag); err == nil {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	res, err := Run(ctx, Spec{Dir: dir, Backend: "wb-ok", Retry: []string{"wb-ok"}, Gate: []string{"touch " + flag + " && sleep 30"},
		Tasks: []Task{{ID: "cut", Prompt: "do the thing"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := res.Tasks[0]
	if tr.Status == StatusPass || len(tr.Attempts) != 1 || len(tr.Attempts[0].Gates) != 1 {
		t.Fatalf("task = %+v", tr)
	}
	g := tr.Attempts[0].Gates[0]
	if g.ExitCode != -1 || g.Killed != KilledCanceled || g.Timeout != 0 || !strings.HasSuffix(g.Output, "[canceled]") || g.Outcome() != "canceled" {
		t.Errorf("gate = %+v, want it recorded as canceled", g)
	}
}

func TestGateOutcome(t *testing.T) {
	for _, tc := range []struct {
		g           GateResult
		want, brief string
	}{
		{GateResult{ExitCode: 0}, "exited 0", "exited 0"},
		{GateResult{ExitCode: 3}, "exited 3", "exited 3"},
		{GateResult{ExitCode: -1}, "exited -1", "exited -1"}, // a record from before the field, or a shell that could not start
		{GateResult{ExitCode: -1, Killed: KilledTimeout, Timeout: agent.Duration(15 * time.Minute)},
			`killed after 15m0s (gate_timeout; set "gate_timeout" in the spec to allow more)`, "was killed after 15m0s by gate_timeout"},
		{GateResult{ExitCode: -1, Killed: KilledCanceled}, "canceled", "was canceled"},
	} {
		if got := tc.g.Outcome(); got != tc.want {
			t.Errorf("%+v: Outcome() = %q, want %q", tc.g, got, tc.want)
		}
		if got := tc.g.Brief(); got != tc.brief {
			t.Errorf("%+v: Brief() = %q, want %q", tc.g, got, tc.brief)
		}
	}
}

func TestGateResultRecordStaysLoadable(t *testing.T) {
	// A record written before the field existed loads with it empty.
	var old GateResult
	if err := json.Unmarshal([]byte(`{"command":"go test ./...","exit_code":-1,"output":"[timed out after 15m0s]","duration_ns":900000000000}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Killed != "" || old.Timeout != 0 || old.Outcome() != "exited -1" {
		t.Errorf("old record = %+v", old)
	}
	// A new record carries the reason and the limit as text, and nothing when
	// the gate exited on its own.
	killed := GateResult{Command: "go test ./...", ExitCode: -1, Killed: KilledTimeout, Timeout: agent.Duration(15 * time.Minute)}
	data, err := json.Marshal(killed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"killed":"timeout","timeout":"15m0s"`) {
		t.Errorf("record = %s", data)
	}
	var back GateResult
	if err := json.Unmarshal(data, &back); err != nil || back != killed {
		t.Errorf("round trip = %+v (%v), want %+v", back, err, killed)
	}
	if data, _ := json.Marshal(GateResult{Command: "go vet", ExitCode: 0}); strings.Contains(string(data), "killed") || strings.Contains(string(data), "timeout") {
		t.Errorf("a gate that exited carries the fields: %s", data)
	}
}

func TestGateTimeoutIsChecked(t *testing.T) {
	dir := initRepo(t)
	for _, tc := range []struct {
		name string
		spec Spec
	}{
		{"negative on the spec", Spec{Dir: dir, Backend: "wb-ok", GateTimeout: agent.Duration(-time.Second), Tasks: []Task{{Prompt: "x"}}}},
		{"over a day on a task", Spec{Dir: dir, Backend: "wb-ok", Tasks: []Task{{Prompt: "x", GateTimeout: agent.Duration(25 * time.Hour)}}}},
	} {
		if err := tc.spec.normalize(); err == nil || !strings.Contains(err.Error(), "gate_timeout") {
			t.Errorf("%s: err = %v, want one naming gate_timeout", tc.name, err)
		}
	}
}

func TestExpectChecksTheFinalAnswerBeforeTheGates(t *testing.T) {
	dir := initRepo(t)
	// wb-noop answers "nothing to do", so the first attempt misses the
	// pattern; wb-ok answers "wrote out.txt" and passes.
	res, events := run(t, Spec{Dir: dir, Backend: "wb-noop", Retry: []string{"wb-ok"},
		Gate: []string{"test -f out.txt"}, Tasks: []Task{{ID: "exp", Prompt: "write it", Expect: `^wrote\b`}}})
	tr := res.Tasks[0]
	if tr.Status != StatusPass || len(tr.Attempts) != 2 {
		t.Fatalf("task = %+v", tr)
	}
	a1 := tr.Attempts[0]
	if len(a1.Gates) != 1 || a1.Gates[0].Command != `expect /^wrote\b/` || a1.Gates[0].ExitCode != 1 {
		t.Fatalf("attempt 1 gates = %+v: a missed expect is reported as a failed gate and stops the rest", a1.Gates)
	}
	if a1.Result.ToolCalls != 1 {
		t.Errorf("attempt 1 tool calls = %d, want the stub's one", a1.Result.ToolCalls)
	}
	for _, want := range []string{`expect /^wrote\b/`, "nothing to do"} {
		if !strings.Contains(tr.Attempts[1].Prompt, want) {
			t.Errorf("the retry must be told what the answer missed; prompt lacks %q:\n%s", want, tr.Attempts[1].Prompt)
		}
	}
	if g := tr.Attempts[1].Gates; len(g) != 2 || g[0].ExitCode != 0 || g[1].Command != "test -f out.txt" {
		t.Errorf("attempt 2 gates = %+v, want expect then the gate", g)
	}
	if gates := eventTexts(events, "gate"); !strings.Contains(gates, "✗ expect") || !strings.Contains(gates, "✓ expect") {
		t.Errorf("gate events = %q", gates)
	}
}

func TestExpectMustCompile(t *testing.T) {
	spec := Spec{Backend: "wb-ok", Tasks: []Task{{Prompt: "a", Expect: "(unclosed"}}}
	if _, err := Run(context.Background(), spec, nil); err == nil || !strings.Contains(err.Error(), "task t1: expect") {
		t.Errorf("err = %v, want the bad pattern refused before anything runs", err)
	}
}

// A reader of the record, such as hrn watch, follows the batch while it goes:
// the attempt names its run while the agent works, the run's result is there
// once it ends, and each gate appears as it finishes.
func TestTheRecordFollowsTheBatchWhileItGoes(t *testing.T) {
	dir := initRepo(t)
	onDisk := func() TaskResult {
		t.Helper()
		results, _, err := List()
		if err != nil || len(results) != 1 {
			t.Fatalf("batch records = %d, %v; want the one this test runs", len(results), err)
		}
		return results[0].Tasks[0]
	}
	var whileRunning TaskResult
	peek = func() { whileRunning = onDisk() }
	t.Cleanup(func() { peek = func() {} })

	var afterRun TaskResult
	var gatesSeen []int
	res, err := Run(context.Background(), Spec{Dir: dir, Backend: "wb-peek", Gate: []string{"exit 0", "exit 0"},
		Tasks: []Task{{ID: "peek", Prompt: "look"}}}, func(e Event) {
		switch e.Kind {
		case "run":
			afterRun = onDisk()
		case "gate":
			tr := onDisk()
			if n := len(tr.Attempts); n > 0 {
				gatesSeen = append(gatesSeen, len(tr.Attempts[n-1].Gates))
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	runID := res.Tasks[0].Attempts[0].RunID
	if whileRunning.Status != StatusRunning || len(whileRunning.Attempts) != 1 || whileRunning.Attempts[0].RunID != runID {
		t.Errorf("while the agent worked the record held %+v; want the task running with its attempt naming run %s", whileRunning, runID)
	}
	if len(whileRunning.Attempts) == 1 && whileRunning.Attempts[0].Result.Status != "" {
		t.Errorf("an attempt still working has no result yet, got %q", whileRunning.Attempts[0].Result.Status)
	}
	if len(afterRun.Attempts) != 1 || afterRun.Attempts[0].Result.Status != agent.StatusOK {
		t.Errorf("after the run the record held %+v; want the run's result", afterRun.Attempts)
	}
	if len(gatesSeen) != 2 || gatesSeen[0] != 1 || gatesSeen[1] != 2 {
		t.Errorf("gates on disk as each one finished = %v, want [1 2]", gatesSeen)
	}
	if final := onDisk(); final.Status != StatusPass || len(final.Attempts[0].Gates) != 2 {
		t.Errorf("final record = %+v", final)
	}
}

// A retry is visible too: the first attempt with its result, the second
// still working, under the task that is still running.
func TestTheRecordShowsARetryWhileItRuns(t *testing.T) {
	dir := initRepo(t)
	var whileRetrying TaskResult
	peek = func() {
		results, _, err := List()
		if err != nil || len(results) != 1 {
			t.Errorf("batch records = %d, %v", len(results), err)
			return
		}
		whileRetrying = results[0].Tasks[0]
	}
	t.Cleanup(func() { peek = func() {} })
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-bad", Retry: []string{"wb-peek"}, Tasks: []Task{{ID: "again", Prompt: "p"}}})
	a := whileRetrying.Attempts
	if whileRetrying.Status != StatusRunning || len(a) != 2 || a[0].Result.Status != agent.StatusError ||
		a[1].RunID != res.Tasks[0].Attempts[1].RunID || a[1].Result.Status != "" {
		t.Fatalf("during the retry the record held %+v", whileRetrying)
	}
}

// hrn watch reads batch records over and over while a batch saves them. On
// Windows a reader used to block the save's rename for as long as it held the
// file, so enough readers (anyone on the network, with --lan) made saves fail.
func TestSavesSucceedWhileManyReadersReadTheRecord(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	res := Result{ID: "20261011-120000-abcdef", Started: time.Now(), Tasks: []TaskResult{{ID: "t1", Status: StatusRunning}}}
	if err := save(res); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = LoadResult(res.ID)
				}
			}
		}()
	}
	var failed int
	for i := 0; i < 50; i++ {
		res.Tasks[0].Error = strings.Repeat("x", i)
		if err := save(res); err != nil {
			if failed == 0 {
				t.Logf("first failure: %v", err)
			}
			failed++
		}
	}
	close(stop)
	wg.Wait()
	if failed > 0 {
		t.Fatalf("%d of 50 saves failed while the record was being read", failed)
	}
	got, err := LoadResult(res.ID)
	if err != nil || len(got.Tasks[0].Error) != 49 {
		t.Fatalf("the last save is not the record: %v", err)
	}
}

func TestBatchRetryOnAnotherBackendDropsTheRoutedModelAndEffort(t *testing.T) {
	dir := initRepo(t)
	if err := os.WriteFile(filepath.Join(runlog.Home(), "route.json"), []byte(`{"implement":{"backend":"wb-bad","model":"routed","effort":"medium"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	res, _ := run(t, Spec{Dir: dir, Worktrees: "fresh", Options: agent.Options{NoSwitch: true},
		Tasks: []Task{{ID: "retry", Prompt: "Implement a widget", Retry: []string{"wb-ok"}}}})
	tr := res.Tasks[0]
	if len(tr.Attempts) != 2 {
		t.Fatalf("attempts: %+v", tr.Attempts)
	}
	first, second := firstRun(t, tr), firstRun(t, TaskResult{ID: tr.ID, Attempts: tr.Attempts[1:]})
	if first.Task.Model != "routed" || first.Task.Effort != "medium" || first.RoutedBy == nil {
		t.Fatalf("first attempt: %+v", first)
	}
	if second.Backend != "wb-ok" || second.Task.Model != "" || second.Task.Effort != "" || second.RoutedBy != nil {
		t.Fatalf("retry kept the routed choice: %+v", second)
	}
}

func TestBatchRoutesByTheTasksDeclaredPurpose(t *testing.T) {
	dir := initRepo(t)
	if err := os.WriteFile(filepath.Join(runlog.Home(), "route.json"), []byte(`{"implement":{"backend":"wb-ok"},"review":{"backend":"wb-noop","model":"routed"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	res, _ := run(t, Spec{Dir: dir, Worktrees: "fresh", Options: agent.Options{NoSwitch: true},
		Tasks: []Task{{ID: "declared", Prompt: "Implement a widget", Purpose: "review"}}})
	rec := firstRun(t, res.Tasks[0])
	if rec.Backend != "wb-noop" || rec.Task.Model != "routed" || rec.RoutedBy == nil || rec.RoutedBy.Category != "review" || !rec.RoutedBy.Declared {
		t.Fatalf("declared purpose did not route: %+v %+v", rec, rec.RoutedBy)
	}
}
