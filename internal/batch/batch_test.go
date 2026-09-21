package batch

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
		_ = os.WriteFile(filepath.Join(task.Dir, "out.txt"), []byte(task.Prompt+"\n"), 0o644)
		return agent.Result{Backend: "wb-ok", Status: agent.StatusOK, Final: "wrote out.txt", Turns: 2}
	}})
	agent.Register(stub{"wb-noop", func(context.Context, agent.Task) agent.Result {
		return agent.Result{Backend: "wb-noop", Status: agent.StatusOK, Final: "nothing to do", Turns: 1}
	}})
	agent.Register(stub{"wb-bad", func(context.Context, agent.Task) agent.Result {
		return agent.Result{Backend: "wb-bad", Status: agent.StatusError, Error: "model exploded", Turns: 1}
	}})
	agent.Register(stub{"wb-block", func(ctx context.Context, _ agent.Task) agent.Result {
		blockStarted <- struct{}{}
		<-ctx.Done()
		return agent.Result{Backend: "wb-block", Status: agent.StatusCanceled, Error: ctx.Err().Error()}
	}})
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

// initRepo makes a repository with one commit and points HX_HOME at a temp dir.
func initRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("HX_HOME", t.TempDir())
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

func TestRunPassesGateAndCommits(t *testing.T) {
	dir := initRepo(t)
	base := git(t, dir, "rev-parse", "HEAD")
	res, events := run(t, Spec{
		Dir: dir, Parallel: 2, Backend: "wb-ok", Gate: []string{"exit 0"},
		Tasks: []Task{{ID: "one", Prompt: "first task"}, {Prompt: "second task"}},
	})
	if res.ID == "" || canon(t, res.Dir) != canon(t, dir) || res.Base != base || len(res.Tasks) != 2 {
		t.Fatalf("result = %+v", res)
	}
	for i, want := range []string{"one", "t2"} {
		tr := res.Tasks[i]
		if tr.ID != want || tr.Status != StatusPass || tr.Branch != "hx/"+want || len(tr.Attempts) != 1 {
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

func TestFailuresRetryOnNextBackend(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{
		Dir: dir, Gate: []string{"exit 0"},
		Tasks: []Task{
			{ID: "recovers", Prompt: "p", Backend: "wb-bad", Retry: []string{"wb-ok"}},
			{ID: "gate-fails", Prompt: "p", Backend: "wb-ok", Gate: []string{"exit 0", "exit 3", "exit 0"}, Retry: []string{"wb-ok"}},
		},
	})
	rec := res.Tasks[0]
	if rec.Status != StatusPass || len(rec.Attempts) != 2 || rec.Attempts[0].Backend != "wb-bad" || rec.Attempts[0].Result.Status != agent.StatusError || rec.Attempts[1].Backend != "wb-ok" {
		t.Fatalf("recovers = %+v", rec)
	}
	if rec.Commit == "" || git(t, dir, "rev-parse", "hx/recovers") != rec.Commit {
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
	if branches := git(t, dir, "branch", "--list", "hx/*"); strings.Count(branches, "hx/") != 2 {
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
	batches := filepath.Join(os.Getenv("HX_HOME"), "batches")
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
	if earlyErr != nil || early.ID != res.ID || len(early.Tasks) != 1 || early.Tasks[0].Status != StatusRunning || early.Tasks[0].Branch != "hx/slow" {
		t.Fatalf("record while running = %+v err=%v", early, earlyErr)
	}
	if saved, err := LoadResult(res.ID); err != nil || saved.Tasks[0].Status != StatusCanceled {
		t.Errorf("final record = %+v err=%v", saved, err)
	}
}

func TestRemoveDeletesWorktreesAndBranches(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-ok", Tasks: []Task{{Prompt: "a"}, {Prompt: "b"}}})
	if err := Remove(res); err != nil {
		t.Fatal(err)
	}
	for _, tr := range res.Tasks {
		if _, err := os.Stat(tr.Worktree); err == nil {
			t.Errorf("worktree %s still exists", tr.Worktree)
		}
	}
	if branches := git(t, dir, "branch", "--list", "hx/*"); branches != "" {
		t.Errorf("branches remain: %s", branches)
	}
	if git(t, dir, "worktree", "list") == "" {
		t.Error("main worktree must survive")
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
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-ok", Retry: []string{"wb-ok"}, Tasks: []Task{{ID: "hooked", Prompt: "a"}}})
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
	git(t, dir, "branch", "hx/taken")
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-ok", Retry: []string{"wb-ok"}, Tasks: []Task{{ID: "taken", Prompt: "a"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusError || tr.Error == "" || len(tr.Attempts) != 0 {
		t.Fatalf("existing branch must fail fast without attempts: %+v", tr)
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
	} {
		if _, err := Run(context.Background(), tc.spec, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
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

func TestSpecNoInstructionsReachesTheTask(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-ok", NoInstructions: true, Tasks: []Task{{Prompt: "a"}}})
	rec, err := runlog.Load(res.Tasks[0].Attempts[0].RunID)
	if err != nil || !rec.Task.NoInstructions {
		t.Fatalf("no_instructions should reach the task: %+v err=%v", rec.Task, err)
	}
}
