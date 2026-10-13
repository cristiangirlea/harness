package batch

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/ws"
)

// hook is what wb-hook does while it "works"; a test sets it.
var hook = func(agent.Task) {}

func init() {
	agent.Register(stub{"wb-hook", func(_ context.Context, task agent.Task) agent.Result {
		hook(task)
		return agent.Result{Backend: "wb-hook", Status: agent.StatusOK, Final: "hooked", Turns: 1}
	}})
}

func setHook(t *testing.T, f func(agent.Task)) {
	t.Helper()
	hook = f
	t.Cleanup(func() { hook = func(agent.Task) {} })
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// slotPath is slot n of the repository at dir.
func slotPath(dir string, n int) string {
	return filepath.Join(dir, ".hrn", "slots", strconv.Itoa(n))
}

func samePathT(t *testing.T, got, want string) {
	t.Helper()
	if !strings.EqualFold(canon(t, got), canon(t, want)) {
		t.Errorf("path %q, want %q", got, want)
	}
}

// quickPoll makes a task waiting for a slot look again soon.
func quickPoll(t *testing.T) {
	t.Helper()
	old := slotPoll
	slotPoll = 20 * time.Millisecond
	t.Cleanup(func() { slotPoll = old })
}

// holdAll leases every slot a batch of parallel 1 would try, from the test.
func holdAll(t *testing.T, dir string) []ws.Taken {
	t.Helper()
	var held []ws.Taken
	for range ws.DefaultMax {
		got, err := ws.Take(context.Background(), ws.TakeOptions{Repo: dir, Holder: "test", NoFetch: true})
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, got)
	}
	return held
}

func TestTwoBatchesInARowReuseTheSlotAndKeepItsBuildOutput(t *testing.T) {
	dir := initRepo(t)
	writeFile(t, filepath.Join(dir, ".gitignore"), "target/\n")
	git(t, dir, "add", ".gitignore")
	git(t, dir, "commit", "-q", "-m", "ignore target")

	type seen struct{ marker, out, readmeChanged bool }
	var saw []seen
	setHook(t, func(task agent.Task) {
		readme, _ := os.ReadFile(filepath.Join(task.Dir, "README.md"))
		saw = append(saw, seen{exists(filepath.Join(task.Dir, "target", "marker")),
			exists(filepath.Join(task.Dir, "out.txt")), strings.Contains(string(readme), "changed")})
		writeFile(t, filepath.Join(task.Dir, "target", "marker"), "built\n")
		writeFile(t, filepath.Join(task.Dir, "out.txt"), "untracked until committed\n")
		writeFile(t, filepath.Join(task.Dir, "README.md"), "changed\n")
	})
	first, _ := run(t, Spec{Dir: dir, Parallel: 1, Backend: "wb-hook", Tasks: []Task{{ID: "one", Prompt: "build"}}})
	second, _ := run(t, Spec{Dir: dir, Parallel: 1, Backend: "wb-hook", Tasks: []Task{{ID: "two", Prompt: "build"}}})
	a, b := first.Tasks[0], second.Tasks[0]
	if a.Status != StatusPass || b.Status != StatusPass {
		t.Fatalf("tasks: %+v / %+v", a, b)
	}
	if a.Slot != 1 || b.Slot != 1 {
		t.Errorf("slots %d and %d, want both in slot 1", a.Slot, b.Slot)
	}
	samePathT(t, a.Worktree, slotPath(dir, 1))
	samePathT(t, b.Worktree, a.Worktree)
	if len(saw) != 2 || saw[0].marker {
		t.Fatalf("seen = %+v", saw)
	}
	if !saw[1].marker {
		t.Error("the second batch should find the first one's ignored build output")
	}
	if saw[1].out || saw[1].readmeChanged {
		t.Errorf("the second batch should not see the first one's files or changes: %+v", saw[1])
	}
	for _, tr := range []TaskResult{a, b} {
		if tr.Commit == "" || git(t, dir, "rev-parse", tr.Branch) != tr.Commit {
			t.Errorf("%s: branch %s should hold commit %q", tr.ID, tr.Branch, tr.Commit)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".hrn", "slots", "1.lease")); !os.IsNotExist(err) {
		t.Errorf("the slot should be given back after the batch: %v", err)
	}
	if exists(filepath.Join(dir, ".hrn", "worktrees", "one")) {
		t.Error("slots mode makes no .hrn/worktrees/<id>")
	}
	if saved, err := LoadResult(first.ID); err != nil || saved.Tasks[0].Slot != 1 {
		t.Errorf("the record should name the slot: %+v, %v", saved.Tasks, err)
	}
}

func TestParallelTasksGetASlotEachAndTheirBranchesMerge(t *testing.T) {
	dir := initRepo(t)
	// Both agents work at once, so each needs a slot of its own.
	var mu sync.Mutex
	arrived := 0
	both := make(chan struct{})
	setHook(t, func(task agent.Task) {
		name := strings.Fields(task.Prompt)[0]
		writeFile(t, filepath.Join(task.Dir, name+".txt"), name+"\n")
		mu.Lock()
		if arrived++; arrived == 2 {
			close(both)
		}
		mu.Unlock()
		select {
		case <-both:
		case <-time.After(10 * time.Second):
		}
	})
	res, _ := run(t, Spec{Dir: dir, Parallel: 2, Backend: "wb-hook",
		Tasks: []Task{{ID: "alpha", Prompt: "alpha"}, {ID: "beta", Prompt: "beta"}}})
	slots := map[int]bool{}
	for _, tr := range res.Tasks {
		if tr.Status != StatusPass {
			t.Fatalf("%s: %+v", tr.ID, tr)
		}
		slots[tr.Slot] = true
		samePathT(t, tr.Worktree, slotPath(dir, tr.Slot))
		if tr.Commit == "" || git(t, dir, "rev-parse", tr.Branch) != tr.Commit {
			t.Errorf("%s: branch should hold commit %q", tr.ID, tr.Commit)
		}
	}
	if !slots[1] || !slots[2] {
		t.Errorf("slots = %v, want 1 and 2", slots)
	}
	merged, err := Merge(context.Background(), res, nil, false)
	if err != nil || len(merged) != 2 || merged[0].Commit == "" || merged[1].Commit == "" {
		t.Fatalf("merged = %+v err=%v", merged, err)
	}
	for _, name := range []string{"alpha.txt", "beta.txt"} {
		if !exists(filepath.Join(dir, name)) {
			t.Errorf("%s did not arrive", name)
		}
	}
}

func TestAFreshRetryInASlotStartsFromTheBaseInTheSameSlot(t *testing.T) {
	dir := initRepo(t)
	var dirs []string
	var leftover bool
	setHook(t, func(task agent.Task) {
		dirs = append(dirs, task.Dir)
		leftover = exists(filepath.Join(task.Dir, "leftover.txt"))
		writeFile(t, filepath.Join(task.Dir, "out.txt"), "second\n")
	})
	res, events := run(t, Spec{Dir: dir, Backend: "wb-bad-writes", Retry: []string{"wb-hook"},
		Tasks: []Task{{ID: "fresh", Prompt: "do it"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusPass || len(tr.Attempts) != 2 || tr.Attempts[1].Continued {
		t.Fatalf("task = %+v", tr)
	}
	if leftover {
		t.Error("the second attempt must not see the first attempt's file")
	}
	if len(dirs) != 1 || tr.Slot != 1 {
		t.Fatalf("hook dirs = %v, slot %d", dirs, tr.Slot)
	}
	samePathT(t, dirs[0], tr.Worktree)
	if n := git(t, dir, "rev-list", "--count", res.Base+"..hrn/fresh"); n != "1" {
		t.Errorf("commits above base = %s, want only the second attempt's", n)
	}
	if retries := eventTexts(events, "retry"); !strings.Contains(retries, "slot reset to the base") {
		t.Errorf("retry events = %q", retries)
	}
}

func TestAFailedSlotTaskSaysItsBranchIsKept(t *testing.T) {
	dir := initRepo(t)
	res, events := run(t, Spec{Dir: dir, Backend: "wb-bad-writes", Tasks: []Task{{ID: "broke", Prompt: "p"}}})
	if res.Tasks[0].Status != StatusFail {
		t.Fatalf("task = %+v", res.Tasks[0])
	}
	if done := eventTexts(events, "done"); !strings.Contains(done, "branch hrn/broke kept") || strings.Contains(done, "worktree kept") {
		t.Errorf("done = %q", done)
	}
	if _, err := os.Stat(filepath.Join(dir, ".hrn", "slots", "1.lease")); !os.IsNotExist(err) {
		t.Errorf("a failed task gives its slot back too: %v", err)
	}
}

func TestATaskWaitsForAFreeSlotAndThenRuns(t *testing.T) {
	dir := initRepo(t)
	quickPoll(t)
	held := holdAll(t, dir)
	var once sync.Once
	var waits []string
	res, err := Run(context.Background(), Spec{Dir: dir, Parallel: 1, Backend: "wb-ok",
		Tasks: []Task{{ID: "patient", Prompt: "p"}}}, func(e Event) {
		if e.Kind != "wait" {
			return
		}
		waits = append(waits, e.Text)
		once.Do(func() {
			go func() {
				time.Sleep(100 * time.Millisecond)
				if err := ws.Release(context.Background(), held[2], ""); err != nil {
					t.Errorf("release: %v", err)
				}
			}()
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	tr := res.Tasks[0]
	if tr.Status != StatusPass || tr.Slot != 3 {
		t.Fatalf("task = %+v, want a pass in the freed slot 3", tr)
	}
	if len(waits) == 0 || !strings.Contains(waits[0], "slot 1") || !strings.Contains(waits[0], `"test"`) {
		t.Errorf("wait events = %q, want them to name the held slots", waits)
	}
	if len(waits) > 1 && waits[1] == waits[0] {
		t.Errorf("the same wait was announced twice: %q", waits)
	}
}

func TestATaskWaitingForASlotIsCanceledWhenTheContextEnds(t *testing.T) {
	dir := initRepo(t)
	quickPoll(t)
	holdAll(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res, err := Run(ctx, Spec{Dir: dir, Parallel: 1, Backend: "wb-ok",
		Tasks: []Task{{ID: "patient", Prompt: "p"}}}, func(e Event) {
		if e.Kind == "wait" {
			cancel()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	tr := res.Tasks[0]
	if tr.Status != StatusCanceled || len(tr.Attempts) != 0 || tr.Slot != 0 {
		t.Fatalf("task = %+v", tr)
	}
	if b := git(t, dir, "branch", "--list", "hrn/patient"); b != "" {
		t.Errorf("a task that never got a slot leaves no branch: %q", b)
	}
	for n := 1; n <= ws.DefaultMax; n++ {
		if _, err := os.Stat(filepath.Join(dir, ".hrn", "slots", strconv.Itoa(n)+".lease")); err != nil {
			t.Errorf("the test's lease on slot %d should stay: %v", n, err)
		}
	}
}

func TestACommitFailureInASlotKeepsItLeasedAndSaysSo(t *testing.T) {
	dir := initRepo(t)
	hookFile := filepath.Join(dir, ".git", "hooks", "pre-commit")
	writeFile(t, hookFile, "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(hookFile, 0o755); err != nil {
		t.Fatal(err)
	}
	res, events := run(t, Spec{Dir: dir, Backend: "wb-ok", Tasks: []Task{{ID: "hooked", Prompt: "a"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusError || !strings.Contains(tr.Error, "uncommitted") || !strings.Contains(tr.Error, "slot 1 kept") {
		t.Fatalf("task = %+v", tr)
	}
	if done := eventTexts(events, "done"); !strings.Contains(done, "slot 1 kept") {
		t.Errorf("done = %q", done)
	}
	if !exists(filepath.Join(tr.Worktree, "out.txt")) {
		t.Error("the agent's work must stay in the slot")
	}
	if !exists(filepath.Join(dir, ".hrn", "slots", "1.lease")) {
		t.Error("the slot holding uncommitted work stays leased")
	}
}

func TestASlotWhoseAgentCommittedOnADetachedHEADIsKeptAndSkipped(t *testing.T) {
	dir := initRepo(t)
	var head string
	setHook(t, func(task agent.Task) {
		git(t, task.Dir, "switch", "-q", "--detach")
		writeFile(t, filepath.Join(task.Dir, "detached.txt"), "x\n")
		git(t, task.Dir, "add", "detached.txt")
		git(t, task.Dir, "commit", "-q", "-m", "detached")
		head = git(t, task.Dir, "rev-parse", "HEAD")
	})
	res, _ := run(t, Spec{Dir: dir, Parallel: 1, Backend: "wb-hook", Tasks: []Task{{ID: "loose", Prompt: "p"}}})
	tr := res.Tasks[0]
	if tr.Slot != 1 || !strings.Contains(tr.Error, "slot 1 kept") || !strings.Contains(tr.Error, "no remote, branch or tag") {
		t.Fatalf("task = %+v", tr)
	}
	if h := git(t, tr.Worktree, "rev-parse", "HEAD"); h != head {
		t.Errorf("the slot's HEAD moved to %s, want the agent's detached commit %s", h, head)
	}
	if !exists(filepath.Join(dir, ".hrn", "slots", "1.lease")) {
		t.Fatal("the slot holding a commit on no ref stays leased")
	}
	os.Remove(filepath.Join(dir, ".hrn", "slots", "1.lease")) // as when hrn exits
	next, err := ws.Take(context.Background(), ws.TakeOptions{Repo: dir, Holder: "next", NoFetch: true})
	if err != nil {
		t.Fatal(err)
	}
	if next.N != 2 || !strings.Contains(strings.Join(next.Notes, "\n"), "slot 1") {
		t.Errorf("the next take should skip slot 1: got slot %d, notes %q", next.N, next.Notes)
	}
}

func TestFreshWorktreesAreMadePerTaskAsBefore(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-ok", Worktrees: "fresh", Tasks: []Task{{ID: "old", Prompt: "p"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusPass || tr.Slot != 0 {
		t.Fatalf("task = %+v", tr)
	}
	samePathT(t, tr.Worktree, filepath.Join(dir, ".hrn", "worktrees", "old"))
	if !exists(filepath.Join(tr.Worktree, "out.txt")) {
		t.Error("a fresh worktree is kept after the batch")
	}
	if exists(filepath.Join(dir, ".hrn", "slots")) {
		t.Error("fresh mode takes no slot")
	}
}

func TestAnUnknownWorktreesValueIsRefused(t *testing.T) {
	_, err := Run(context.Background(), Spec{Backend: "wb-ok", Worktrees: "reuse", Tasks: []Task{{Prompt: "a"}}}, nil)
	if err == nil || !strings.Contains(err.Error(), `worktrees "reuse"`) || !strings.Contains(err.Error(), "fresh") {
		t.Errorf("err = %v", err)
	}
}

func TestDiffOfASlotTaskReadsOnlyItsCommitNeverTheSlotItLeft(t *testing.T) {
	dir := initRepo(t)
	quiet, _ := run(t, Spec{Dir: dir, Parallel: 1, Backend: "wb-noop", Tasks: []Task{{ID: "quiet", Prompt: "look"}}})
	var diff string
	var untracked []string
	var diffErr error
	setHook(t, func(task agent.Task) {
		// Another task holds the slot now and has work in it.
		writeFile(t, filepath.Join(task.Dir, "stray.txt"), "not quiet's\n")
		writeFile(t, filepath.Join(task.Dir, "README.md"), "changed\n")
		diff, untracked, diffErr = Diff(context.Background(), quiet, "quiet")
	})
	busy, _ := run(t, Spec{Dir: dir, Parallel: 1, Backend: "wb-hook", Tasks: []Task{{ID: "busy", Prompt: "work"}}})
	samePathT(t, busy.Tasks[0].Worktree, quiet.Tasks[0].Worktree)
	if diffErr != nil || diff != "" || len(untracked) != 0 {
		t.Errorf("a slot task that committed nothing has an empty diff, got %q %v %v", diff, untracked, diffErr)
	}
	got, _, err := Diff(context.Background(), busy, "busy")
	if err != nil || !strings.Contains(got, "stray.txt") {
		t.Errorf("a committed slot task's diff = %q, %v", got, err)
	}
}

func TestRemoveOfSlotTasksDeletesOnlyTheirBranches(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{Dir: dir, Parallel: 1, Backend: "wb-file",
		Tasks: []Task{{ID: "one", Prompt: "one"}, {ID: "two", Prompt: "two"}}})
	// Someone else holds a slot meanwhile, and slot 1 has hrn/one checked out
	// again, as a task still running would.
	other, err := ws.Take(context.Background(), ws.TakeOptions{Repo: dir, Holder: "someone", NoFetch: true})
	if err != nil {
		t.Fatal(err)
	}
	busy, err := ws.Take(context.Background(), ws.TakeOptions{Repo: dir, Branch: "hrn/one", Holder: "still running", NoFetch: true})
	if err != nil {
		t.Fatal(err)
	}
	err = Remove(res)
	if err == nil || !strings.Contains(err.Error(), "hrn/one") || !strings.Contains(err.Error(), "checked out") {
		t.Fatalf("Remove must refuse a branch checked out in a slot: %v", err)
	}
	if b := git(t, dir, "branch", "--list", "hrn/one"); b == "" {
		t.Error("the checked-out branch must stay")
	}
	if b := git(t, dir, "branch", "--list", "hrn/two"); b != "" {
		t.Errorf("the other task's branch should still go: %q", b)
	}

	if err := ws.Release(context.Background(), busy, ""); err != nil {
		t.Fatal(err)
	}
	if err := Remove(res); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if b := git(t, dir, "branch", "--list", "hrn/*"); b != "" {
		t.Errorf("branches remain: %s", b)
	}
	for _, tr := range res.Tasks {
		if !exists(tr.Worktree) {
			t.Errorf("slot %s must stay", tr.Worktree)
		}
	}
	if !exists(filepath.Join(dir, ".hrn", "slots", strconv.Itoa(other.N)+".lease")) {
		t.Error("someone else's lease must stay")
	}
}

// hrn prune keeps a batch record while hrn batch rm still has something to
// remove. A slot outlives every batch, so for a slot task that is its branch.
func TestASlotBatchRemainsWhileItsBranchesDo(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-ok", Tasks: []Task{{ID: "one", Prompt: "p"}}})
	if !WorktreesRemain(res) {
		t.Error("the batch's branch is still there, so its record must stay")
	}
	if err := Remove(res); err != nil {
		t.Fatal(err)
	}
	if WorktreesRemain(res) {
		t.Error("after batch rm only the slot is left, which is not the batch's")
	}
}

// Slots a batch keeps (their commit failed) never come free while it runs, so
// a task that finds only those, or slots skipped for what is in them, fails
// instead of waiting for ever.
func TestATaskFailsRatherThanWaitForSlotsOnlyItsBatchKept(t *testing.T) {
	dir := initRepo(t)
	quickPoll(t)
	hookFile := filepath.Join(dir, ".git", "hooks", "pre-commit")
	writeFile(t, hookFile, "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(hookFile, 0o755); err != nil {
		t.Fatal(err)
	}
	var tasks []Task
	for i := range ws.DefaultMax + 1 {
		tasks = append(tasks, Task{ID: "t" + strconv.Itoa(i+1), Prompt: "p"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := Run(ctx, Spec{Dir: dir, Parallel: 1, Backend: "wb-ok", Tasks: tasks}, nil)
	if err != nil {
		t.Fatal(err)
	}
	last := res.Tasks[len(res.Tasks)-1]
	if last.Status != StatusError || !strings.Contains(last.Error, "none will come free") || !strings.Contains(last.Error, "slot 1") {
		t.Fatalf("last task = %s: %s", last.Status, last.Error)
	}
	if b := git(t, dir, "branch", "--list", last.Branch); b != "" {
		t.Errorf("a task that got no slot leaves no branch: %q", b)
	}
}

func TestATaskCanceledWhileItRunsGivesItsSlotBack(t *testing.T) {
	dir := initRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-blockStarted
		cancel()
	}()
	res, err := Run(ctx, Spec{Dir: dir, Parallel: 1, Backend: "wb-block", Tasks: []Task{{ID: "stopped", Prompt: "p"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := res.Tasks[0]
	if tr.Status != StatusCanceled || tr.Slot != 1 {
		t.Fatalf("task = %+v", tr)
	}
	if _, err := os.Stat(filepath.Join(dir, ".hrn", "slots", "1.lease")); !os.IsNotExist(err) {
		t.Errorf("a canceled task gives its slot back: %v", err)
	}
	if b := git(t, tr.Worktree, "branch", "--show-current"); b != "" {
		t.Errorf("the slot should be detached, is on %q", b)
	}
}
