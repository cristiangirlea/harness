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

func TestNoInstructionsReachesTheTask(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-ok", Tasks: []Task{
		{ID: "spec-off", Prompt: "a"},
		{ID: "task-off", Prompt: "a", NoInstructions: true},
	}})
	for _, tc := range []struct {
		i    int
		want bool
	}{{0, false}, {1, true}} {
		rec, err := runlog.Load(res.Tasks[tc.i].Attempts[0].RunID)
		if err != nil || rec.Task.NoInstructions != tc.want {
			t.Errorf("%s: NoInstructions = %v, want %v (err=%v)", res.Tasks[tc.i].ID, rec.Task.NoInstructions, tc.want, err)
		}
	}

	dir = initRepo(t)
	res, _ = run(t, Spec{Dir: dir, Backend: "wb-ok", NoInstructions: true, Tasks: []Task{{Prompt: "a"}}})
	if rec, err := runlog.Load(res.Tasks[0].Attempts[0].RunID); err != nil || !rec.Task.NoInstructions {
		t.Errorf("the spec-wide switch should reach every task: %+v err=%v", rec.Task, err)
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
	if n := git(t, dir, "rev-list", "--count", res.Base+"..hx/keeps"); n != "2" {
		t.Errorf("commits above base = %s, want 2 (the first attempt's work must survive)", n)
	}
	if subject := git(t, dir, "log", "-1", "--format=%s", "hx/keeps"); !strings.HasSuffix(subject, "(attempt 2)") {
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
	if n := git(t, dir, "rev-list", "--count", res.Base+"..hx/fresh"); n != "1" {
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
	if n := git(t, dir, "rev-list", "--count", res.Base+"..hx/third"); n != "3" {
		t.Errorf("commits above base = %s, want 3", n)
	}
	if subject := git(t, dir, "log", "-1", "--format=%s", "hx/third"); !strings.HasSuffix(subject, "(attempt 3)") {
		t.Errorf("last commit subject = %q", subject)
	}
}

func TestLimitsReachTheTaskAndAreValidated(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-ok", Timeout: "5m", MaxCostUSD: 2.5, TaskBudget: 30000,
		Tasks: []Task{
			{ID: "from-spec", Prompt: "a"},
			{ID: "own", Prompt: "a", Timeout: "90s", MaxCostUSD: 1, TaskBudget: 40000},
		}})
	for _, tc := range []struct {
		i       int
		timeout time.Duration
		cost    float64
		budget  int
	}{
		{0, 5 * time.Minute, 2.5, 30000},
		{1, 90 * time.Second, 1, 40000},
	} {
		rec, err := runlog.Load(res.Tasks[tc.i].Attempts[0].RunID)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Task.Timeout != tc.timeout || rec.Task.MaxCostUSD != tc.cost || rec.Task.TaskBudget != tc.budget {
			t.Errorf("%s: timeout=%s cost=%v budget=%d", res.Tasks[tc.i].ID, rec.Task.Timeout, rec.Task.MaxCostUSD, rec.Task.TaskBudget)
		}
	}

	for _, tc := range []struct {
		spec Spec
		want string
	}{
		{Spec{Backend: "wb-ok", Timeout: "5 minutes", Tasks: []Task{{Prompt: "a"}}}, "timeout"},
		{Spec{Backend: "wb-ok", TaskBudget: 100, Tasks: []Task{{Prompt: "a"}}}, "20000"},
		{Spec{Backend: "wb-ok", Tasks: []Task{{Prompt: "a", Timeout: "soon"}}}, "task t1"},
		{Spec{Backend: "wb-ok", Tasks: []Task{{Prompt: "a", TaskBudget: 5}}}, "task t1"},
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
		{ID: "opted-out", Prompt: "a", NoContextEdits: true},
	}})
	for i, want := range []bool{false, true} {
		rec, err := runlog.Load(res.Tasks[i].Attempts[0].RunID)
		if err != nil || rec.Task.NoContextEdits != want {
			t.Errorf("%s: NoContextEdits = %v, want %v (err=%v)", res.Tasks[i].ID, rec.Task.NoContextEdits, want, err)
		}
	}
}
