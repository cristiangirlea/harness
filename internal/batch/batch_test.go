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
	agent.Register(stub{"wb-file", func(_ context.Context, task agent.Task) agent.Result {
		name := strings.Fields(task.Prompt)[0] + ".txt"
		_ = os.WriteFile(filepath.Join(task.Dir, name), []byte(task.Prompt+"\n"), 0o644)
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
		Dir: dir, Parallel: 2, Backend: "wb-ok", Gate: []string{"exit 0"},
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
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-ok", Tasks: []Task{{Prompt: "a"}, {Prompt: "b"}}})
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
	git(t, dir, "branch", "hrn/taken")
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-ok", Retry: []string{"wb-ok"}, Tasks: []Task{{ID: "taken", Prompt: "a"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusError || tr.Error == "" || len(tr.Attempts) != 0 {
		t.Fatalf("existing branch must fail fast without attempts: %+v", tr)
	}
}

func TestDirtyRepositoryIsRefused(t *testing.T) {
	dir := initRepo(t)
	spec := func(allowDirty bool) Spec {
		return Spec{Dir: dir, Backend: "wb-ok", AllowDirty: allowDirty, Tasks: []Task{{ID: "t", Prompt: "a"}}}
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
	res, events := run(t, Spec{Dir: dir, Backend: "wb-bad-writes", Retry: []string{"wb-ok"},
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

func mergeable(t *testing.T, dir string, tasks ...Task) Result {
	t.Helper()
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-file", Tasks: tasks})
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
	res := mergeable(t, dir, Task{ID: "alpha", Prompt: "alpha"})
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
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-file", Tasks: []Task{{ID: "alpha", Prompt: "alpha"}}})
	if res.Tasks[0].Commit != "" {
		t.Fatalf("setup: the commit should have failed: %+v", res.Tasks[0])
	}
	diff, _, err := Diff(context.Background(), res, "alpha")
	if err != nil || !strings.Contains(diff, "alpha.txt") {
		t.Fatalf("worktree diff = %q err=%v", diff, err)
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
