package batch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// holdAll leases every slot a batch of parallel 1 would try, from the test,
// as another batch does: with a live pid, this process's.
func holdAll(t *testing.T, dir string) []ws.Taken {
	t.Helper()
	var held []ws.Taken
	for range ws.DefaultMax {
		got, err := ws.Take(context.Background(), ws.TakeOptions{Repo: dir, Holder: "test", PID: os.Getpid(), NoFetch: true})
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
			// The record says the branch is this task's before it waits.
			saved, _, err := List()
			if err != nil || len(saved) != 1 || !saved[0].Tasks[0].BranchMade || saved[0].Tasks[0].Status != StatusRunning {
				t.Errorf("record while waiting = %+v (%v), want a running task with branch_made", saved, err)
			}
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

// Sessions' leases (hrn ws take, no pid) are held until hrn ws free, so a
// batch takes as many slots more as they hold rather than wait on a person.
func TestABatchRunsBesideSlotsSessionsHoldWithoutWaiting(t *testing.T) {
	dir := initRepo(t)
	quickPoll(t)
	for range ws.DefaultMax {
		if _, err := ws.Take(context.Background(), ws.TakeOptions{Repo: dir, Holder: "session", NoFetch: true}); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	arrived := 0
	both := make(chan struct{})
	setHook(t, func(agent.Task) {
		mu.Lock()
		if arrived++; arrived == 2 {
			close(both)
		}
		mu.Unlock()
		select {
		case <-both:
		case <-time.After(10 * time.Second):
			t.Error("the two tasks did not run at once")
		}
	})
	var waits []string
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := Run(ctx, Spec{Dir: dir, Parallel: 2, Backend: "wb-hook",
		Tasks: []Task{{ID: "alpha", Prompt: "a"}, {ID: "beta", Prompt: "b"}}}, func(e Event) {
		if e.Kind == "wait" {
			mu.Lock()
			waits = append(waits, e.Text)
			mu.Unlock()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(waits) != 0 {
		t.Errorf("a batch should not wait on sessions' slots: %q", waits)
	}
	slots := map[int]bool{}
	for _, tr := range res.Tasks {
		if tr.Status != StatusPass {
			t.Fatalf("%s: %+v", tr.ID, tr)
		}
		slots[tr.Slot] = true
	}
	if !slots[ws.DefaultMax+1] || !slots[ws.DefaultMax+2] {
		t.Errorf("slots = %v, want %d and %d", slots, ws.DefaultMax+1, ws.DefaultMax+2)
	}
	for _, n := range []int{ws.DefaultMax + 1, ws.DefaultMax + 2} {
		if exists(filepath.Join(dir, ".hrn", "slots", strconv.Itoa(n)+".lease")) {
			t.Errorf("slot %d should be given back", n)
		}
	}
	for n := 1; n <= ws.DefaultMax; n++ {
		if !exists(filepath.Join(dir, ".hrn", "slots", strconv.Itoa(n)+".lease")) {
			t.Errorf("the session's lease on slot %d should stay", n)
		}
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
	if tr.Status != StatusCanceled || len(tr.Attempts) != 0 || tr.Slot != 0 || tr.BranchMade {
		t.Fatalf("task = %+v, want no branch_made once its branch is dropped", tr)
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
	failCommits(t, dir)
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

// takeSlot makes a task's branch before a slot is free, so a record saved by
// an hrn that crashed while the task waited names no worktree, yet the branch
// is there: batch rm removes it and prune keeps the record until then.
func TestABranchMadeWhileItsTaskWaitedForASlotIsRemovedAndKeepsTheRecord(t *testing.T) {
	dir := initRepo(t)
	git(t, dir, "branch", "hrn/waiting")
	res := Result{ID: "crashed", Dir: dir, Tasks: []TaskResult{{ID: "waiting", Branch: "hrn/waiting", Status: StatusRunning, BranchMade: true, Attempts: []Attempt{}}}}
	if !WorktreesRemain(res) {
		t.Error("the waiting task's branch is still there, so its record must stay")
	}
	busy, err := ws.Take(context.Background(), ws.TakeOptions{Repo: dir, Branch: "hrn/waiting", Holder: "someone", NoFetch: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := Remove(res); err == nil || !strings.Contains(err.Error(), "checked out") {
		t.Errorf("Remove must refuse a branch checked out in a worktree: %v", err)
	}
	if err := ws.Release(context.Background(), busy, ""); err != nil {
		t.Fatal(err)
	}
	if err := Remove(res); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if b := git(t, dir, "branch", "--list", "hrn/waiting"); b != "" {
		t.Errorf("the branch should be gone: %q", b)
	}
	if WorktreesRemain(res) {
		t.Error("with the branch gone the record may go")
	}
}

// Run saves every task as running before any branch exists, so a batch that
// crashed then, before it found hrn/<id> taken, has a running slotless task
// that never made a branch: another batch's branch by that name stays, and
// does not keep this record.
func TestRemoveLeavesAnotherBatchsBranchForATaskThatNeverMadeIt(t *testing.T) {
	dir := initRepo(t)
	git(t, dir, "branch", "hrn/same")
	res := Result{ID: "crashed-early", Dir: dir, Tasks: []TaskResult{{ID: "same", Branch: "hrn/same", Status: StatusRunning, Attempts: []Attempt{}}}}
	if WorktreesRemain(res) {
		t.Error("another batch's branch must not keep this record")
	}
	if err := Remove(res); err != nil {
		t.Fatal(err)
	}
	if b := git(t, dir, "branch", "--list", "hrn/same"); b == "" {
		t.Error("another batch's branch must stay")
	}
}

// A task that ended without a worktree made no branch: one that was there
// already (another batch's) is what failed it, and stays.
func TestRemoveLeavesTheBranchThatFailedATaskWithoutAWorktree(t *testing.T) {
	dir := initRepo(t)
	git(t, dir, "branch", "hrn/taken")
	res := Result{ID: "collided", Dir: dir, Tasks: []TaskResult{{ID: "taken", Branch: "hrn/taken", Status: StatusError,
		Error: "branch hrn/taken already exists; remove it or pick another task id", Attempts: []Attempt{}}}}
	if err := Remove(res); err != nil {
		t.Fatal(err)
	}
	if b := git(t, dir, "branch", "--list", "hrn/taken"); b == "" {
		t.Error("another batch's branch must stay")
	}
	if WorktreesRemain(res) {
		t.Error("nothing of this batch remains")
	}
}

// Slots a batch keeps (their commit failed) never come free while it runs, so
// a task that finds only those, or slots skipped for what is in them, fails
// instead of waiting for ever.
func TestATaskFailsRatherThanWaitForSlotsOnlyItsBatchKept(t *testing.T) {
	dir := initRepo(t)
	quickPoll(t)
	failCommits(t, dir)
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
	if last.Status != StatusError || !strings.Contains(last.Error, "already holds 5 slots") || !strings.Contains(last.Error, "none will come free") {
		t.Fatalf("last task = %s: %s", last.Status, last.Error)
	}
	if b := git(t, dir, "branch", "--list", last.Branch); b != "" {
		t.Errorf("a task that got no slot leaves no branch: %q", b)
	}
}

func TestParallelAboveTenNeverTakesSlotEleven(t *testing.T) {
	dir := initRepo(t)
	failCommits(t, dir)
	var tasks []Task
	for i := range 11 {
		tasks = append(tasks, Task{ID: "t" + strconv.Itoa(i+1), Prompt: "p"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := Run(ctx, Spec{Dir: dir, Parallel: 12, Backend: "wb-ok", Tasks: tasks}, nil)
	if err != nil {
		t.Fatal(err)
	}
	used, denied := 0, 0
	for _, tr := range res.Tasks {
		if tr.Slot > 10 {
			t.Errorf("task %s took slot %d", tr.ID, tr.Slot)
		}
		if tr.Slot > 0 {
			used++
		} else if strings.Contains(tr.Error, "10 slots") {
			denied++
		}
	}
	if used != 10 || denied != 1 {
		t.Errorf("used %d slots, denied %d tasks; want 10 and 1", used, denied)
	}
	if exists(slotPath(dir, 11)) {
		t.Error("slot 11 was made")
	}
}

func TestSessionsAtLimitLeaveBatchWithoutASlot(t *testing.T) {
	dir := initRepo(t)
	for n := 1; n <= ws.Limit; n++ {
		data, err := json.Marshal(ws.Lease{Holder: "session", Taken: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, ".hrn", "slots", strconv.Itoa(n)+".lease"), string(data))
	}
	res, _ := run(t, Spec{Dir: dir, Parallel: 1, Backend: "wb-ok", Tasks: []Task{{ID: "blocked", Prompt: "p"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusError || tr.Slot != 0 || !strings.Contains(tr.Error, "at most 10 slots per repository") {
		t.Fatalf("task = %+v, want no slot and the limit named", tr)
	}
	if exists(slotPath(dir, 11)) {
		t.Error("slot 11 was made")
	}
}

// failCommits sets an invalid commit.cleanup for every commit, so each task's
// slot is kept, or only for the branches named. Batch commits disable hooks
// and commit signing.
func failCommits(t *testing.T, dir string, branches ...string) {
	t.Helper()
	config := filepath.Join(t.TempDir(), "commit.config")
	git(t, dir, "config", "--file", config, "commit.cleanup", "invalid")
	if len(branches) == 0 {
		git(t, dir, "config", "include.path", filepath.ToSlash(config))
		return
	}
	for _, branch := range branches {
		git(t, dir, "config", "includeIf.onbranch:"+branch+".path", filepath.ToSlash(config))
	}
}

// session leases slot n as hrn ws take does for a person: no pid, held
// until hrn ws free.
func session(t *testing.T, dir string) ws.Taken {
	t.Helper()
	got, err := ws.Take(context.Background(), ws.TakeOptions{Repo: dir, Holder: "session", NoFetch: true, Max: ws.Limit})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func leaseHolder(t *testing.T, dir string, n int) (ws.Lease, bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, ".hrn", "slots", strconv.Itoa(n)+".lease"))
	if err != nil {
		return ws.Lease{}, false
	}
	var l ws.Lease
	if err := json.Unmarshal(raw, &l); err != nil {
		t.Fatalf("slot %d lease: %v", n, err)
	}
	return l, true
}

// A session's slot never comes free while a batch runs, and neither do the
// slots the batch kept: a task that finds only those fails at once rather
// than wait for a lease no one will give back.
func TestATaskFailsRatherThanWaitOnASessionOnceItsBatchHoldsItsBound(t *testing.T) {
	dir := initRepo(t)
	quickPoll(t)
	failCommits(t, dir)
	session(t, dir) // slot 1
	var tasks []Task
	for i := range ws.DefaultMax + 1 {
		tasks = append(tasks, Task{ID: "t" + strconv.Itoa(i+1), Prompt: "p"})
	}
	var waits []string
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := Run(ctx, Spec{Dir: dir, Parallel: 1, Backend: "wb-ok", Tasks: tasks}, func(e Event) {
		if e.Kind == "wait" {
			waits = append(waits, e.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range res.Tasks[:ws.DefaultMax] {
		if tr.Status != StatusError || tr.Slot < 2 || tr.Slot > ws.DefaultMax+1 || !strings.Contains(tr.Error, "kept") {
			t.Errorf("%s = %+v, want a kept slot past the session's", tr.ID, tr)
		}
	}
	last := res.Tasks[ws.DefaultMax]
	if last.Status != StatusError || !strings.Contains(last.Error, "already holds 5 slots") {
		t.Errorf("last task = %s: %s, want it to fail on the batch's bound", last.Status, last.Error)
	}
	if len(waits) != 0 {
		t.Errorf("nothing could come free, so the task should not have waited: %q", waits)
	}
}

// Sessions that free their slots while a batch runs shrink the range it
// tries, but not what it already holds: with its kept slots above the new
// range, the batch still holds no more than its bound.
func TestABatchHoldsNoMoreThanItsBoundWhenSessionsFreeTheirSlots(t *testing.T) {
	dir := initRepo(t)
	quickPoll(t)
	failCommits(t, dir)
	for range ws.DefaultMax {
		session(t, dir)
	}
	var tasks []Task
	for i := range 2 * ws.DefaultMax {
		tasks = append(tasks, Task{ID: "t" + strconv.Itoa(i+1), Prompt: "p"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := Run(ctx, Spec{Dir: dir, Parallel: 1, Backend: "wb-ok", Tasks: tasks}, func(e Event) {
		if e.Kind == "done" && e.Task == "t"+strconv.Itoa(ws.DefaultMax) {
			for n := 1; n <= ws.DefaultMax; n++ { // as hrn ws free does
				if err := os.Remove(filepath.Join(dir, ".hrn", "slots", strconv.Itoa(n)+".lease")); err != nil {
					t.Error(err)
				}
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range res.Tasks[:ws.DefaultMax] {
		if tr.Status != StatusError || tr.Slot <= ws.DefaultMax || !strings.Contains(tr.Error, "kept") {
			t.Errorf("%s = %+v, want a kept slot past the sessions'", tr.ID, tr)
		}
	}
	for _, tr := range res.Tasks[ws.DefaultMax:] {
		if tr.Status != StatusError || tr.Slot != 0 || !strings.Contains(tr.Error, "already holds 5 slots") {
			t.Errorf("%s = %+v, want it to fail on the batch's bound", tr.ID, tr)
		}
	}
	for n := 1; n <= ws.DefaultMax; n++ {
		if l, ok := leaseHolder(t, dir, n); ok {
			t.Errorf("slot %d is held by %q; the batch must not take the sessions' freed slots past its bound", n, l.Holder)
		}
	}
}

// Only sessions among the slots a batch would try widen its range: one on
// a slot further up does not let it take a slot past its bound.
func TestASessionOnADistantSlotDoesNotWidenABatchsRange(t *testing.T) {
	dir := initRepo(t)
	quickPoll(t)
	holdAll(t, dir) // another batch on slots 1 to 5
	far := 10
	data, _ := json.Marshal(ws.Lease{Holder: "session", Taken: time.Now()})
	writeFile(t, filepath.Join(dir, ".hrn", "slots", strconv.Itoa(far)+".lease"), string(data))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waited := false
	res, err := Run(ctx, Spec{Dir: dir, Parallel: 1, Backend: "wb-ok",
		Tasks: []Task{{ID: "patient", Prompt: "p"}}}, func(e Event) {
		if e.Kind == "wait" {
			waited = true
			cancel()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	tr := res.Tasks[0]
	if !waited || tr.Status != StatusCanceled || tr.Slot != 0 {
		t.Fatalf("task = %+v (waited %v), want it to wait on the other batch's slots", tr, waited)
	}
	if exists(slotPath(dir, ws.DefaultMax+1)) || exists(filepath.Join(dir, ".hrn", "slots", strconv.Itoa(ws.DefaultMax+1)+".lease")) {
		t.Errorf("slot %d was made: the distant session widened the range", ws.DefaultMax+1)
	}
}

// A session can take a slot after a task's range is worked out and before
// the take scans that slot. When sessions then fill the range, the task
// works it out again and tries the next slot rather than fail.
func TestATaskTriesTheNextSlotWhenASessionTakesOneAfterItsRangeWasWorkedOut(t *testing.T) {
	dir := initRepo(t)
	quickPoll(t)
	failCommits(t, dir)
	var tasks []Task
	for i := range ws.DefaultMax {
		tasks = append(tasks, Task{ID: "t" + strconv.Itoa(i+1), Prompt: "p"})
	}
	taken := false
	rangeComputed = func(max int) {
		// The last task's try: the ones before it kept slots 1 to 4, so its
		// range ends at slot 5, which a session takes before the take scans it.
		if _, ok := leaseHolder(t, dir, ws.DefaultMax-1); !ok || taken {
			return
		}
		taken = true
		if max != ws.DefaultMax {
			t.Errorf("range = %d, want %d", max, ws.DefaultMax)
		}
		session(t, dir)
	}
	t.Cleanup(func() { rangeComputed = nil })
	res, events := run(t, Spec{Dir: dir, Parallel: 1, Backend: "wb-ok", Tasks: tasks})
	if !taken {
		t.Fatal("the session never took its slot")
	}
	if l, ok := leaseHolder(t, dir, ws.DefaultMax); !ok || l.Holder != "session" {
		t.Fatalf("slot %d lease = %+v (%v), want the session's", ws.DefaultMax, l, ok)
	}
	last := res.Tasks[ws.DefaultMax-1]
	if last.Slot != ws.DefaultMax+1 || !strings.Contains(last.Error, "kept") || strings.Contains(last.Error, "none will come free") {
		t.Fatalf("last task = %+v, want it to run in slot %d past the session's", last, ws.DefaultMax+1)
	}
	if w := eventTexts(events, "wait"); w != "" {
		t.Errorf("the task should not wait: %q", w)
	}
}

// At its bound, a batch waits for the slots its own running tasks hold,
// which come back when they end; only kept slots never do.
func TestATaskAtTheBatchsBoundWaitsForItsRunningTasksNotItsKeptSlots(t *testing.T) {
	dir := initRepo(t)
	quickPoll(t)
	failCommits(t, dir, "hrn/t1")
	var mu sync.Mutex
	var waits []string
	t6Waited := make(chan struct{})
	setHook(t, func(task agent.Task) {
		writeFile(t, filepath.Join(task.Dir, "out.txt"), "ok\n")
		if strings.HasPrefix(task.Prompt, "t1") {
			return // its commit fails, so its slot is kept
		}
		select {
		case <-t6Waited:
		case <-time.After(10 * time.Second):
			t.Error("t6 did not wait for a running task's slot")
		}
	})
	tasks := []Task{{ID: "t1", Prompt: "t1"}, {ID: "t2", Prompt: "t2"}, {ID: "t3", Prompt: "t3"}, {ID: "t4", Prompt: "t4"}, {ID: "t5", Prompt: "t5"}, {ID: "t6", Prompt: "t6"}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var once sync.Once
	res, err := Run(ctx, Spec{Dir: dir, Parallel: ws.DefaultMax, Backend: "wb-hook", Tasks: tasks}, func(e Event) {
		if e.Kind == "wait" && e.Task == "t6" {
			mu.Lock()
			waits = append(waits, e.Text)
			mu.Unlock()
			once.Do(func() { close(t6Waited) })
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]TaskResult{}
	for _, tr := range res.Tasks {
		byID[tr.ID] = tr
	}
	if tr := byID["t1"]; tr.Status != StatusError || !strings.Contains(tr.Error, "kept") {
		t.Errorf("t1 = %+v, want its slot kept", tr)
	}
	if tr := byID["t6"]; tr.Status != StatusPass {
		t.Errorf("t6 = %+v, want it to run once a running task gave its slot back", tr)
	}
	if len(waits) == 0 || !strings.Contains(waits[0], "this batch holds") {
		t.Errorf("t6's waits = %q, want one on the batch's own slots", waits)
	}
}

// Slots skipped for what is in them are never cleaned by a batch, so a task
// that finds only those fails rather than wait.
func TestATaskFailsRatherThanWaitForSkippedSlots(t *testing.T) {
	dir := initRepo(t)
	quickPoll(t)
	for _, h := range holdAll(t, dir) {
		if err := ws.Release(context.Background(), h, ""); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(h.Path, "stray.txt"), "left behind\n")
	}
	res, events := run(t, Spec{Dir: dir, Parallel: 1, Backend: "wb-ok", Tasks: []Task{{ID: "stuck", Prompt: "p"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusError || !strings.Contains(tr.Error, "none will come free") || !strings.Contains(tr.Error, "stray.txt") {
		t.Fatalf("task = %+v", tr)
	}
	if w := eventTexts(events, "wait"); w != "" {
		t.Errorf("the task should not wait: %q", w)
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

// The record stops claiming the branch before takeSlot drops it, so a crash
// between the two never leaves a record that would let hrn batch rm delete
// a branch of the same name another batch made since.
func TestATaskWaitingForASlotSavesItNoLongerOwnsItsBranchBeforeDroppingIt(t *testing.T) {
	dir := initRepo(t)
	quickPoll(t)
	holdAll(t, dir)
	dropped := 0
	droppingBranch = func() {
		dropped++
		saved, _, err := List()
		if err != nil || len(saved) != 1 || saved[0].Tasks[0].BranchMade {
			t.Errorf("record as the branch is dropped = %+v (%v), want branch_made cleared", saved, err)
		}
	}
	t.Cleanup(func() { droppingBranch = nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := Run(ctx, Spec{Dir: dir, Parallel: 1, Backend: "wb-ok",
		Tasks: []Task{{ID: "patient", Prompt: "p"}}}, func(e Event) {
		if e.Kind == "wait" {
			cancel()
		}
	}); err != nil {
		t.Fatal(err)
	}
	if dropped != 1 {
		t.Errorf("branch dropped %d times, want 1", dropped)
	}
}

// When the record cannot say the branch is no longer the task's, the branch
// stays and the task says so: a record claiming a missing branch could later
// let hrn batch rm delete someone else's.
func TestATaskWaitingForASlotLeavesItsBranchWhenItCannotSaveThatItDropsIt(t *testing.T) {
	dir := initRepo(t)
	quickPoll(t)
	holdAll(t, dir)
	claimed := false // fail only the save that takes back the claim on the branch
	failSave = func(res Result) error {
		tr := res.Tasks[0]
		if tr.Status == StatusRunning && tr.BranchMade {
			claimed = true
		} else if claimed && tr.Status == StatusRunning {
			return errors.New("disk full")
		}
		return nil
	}
	t.Cleanup(func() { failSave = nil })
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
	if tr.Status != StatusCanceled || !tr.BranchMade || !strings.Contains(tr.Error, "branch hrn/patient is left") || !strings.Contains(tr.Error, "disk full") {
		t.Fatalf("task = %+v, want a canceled task that still owns its branch and says it is left", tr)
	}
	if b := git(t, dir, "branch", "--list", "hrn/patient"); b == "" {
		t.Error("the branch must stay when the record could not be saved")
	}
}

// TestHelperExit is a process that ends once its stdin closes, so its pid names a process
// that has exited.
func TestHelperExit(t *testing.T) {
	if os.Getenv("HRN_BATCH_HELPER") != "exit" {
		return
	}
	var hold [1]byte
	_, _ = os.Stdin.Read(hold[:])
	os.Exit(0)
}

// exitedPID records the helper's start identity while it is held running,
// then lets it exit and returns its pid and start identity.
func exitedPID(t *testing.T) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperExit$")
	cmd.Env = append(os.Environ(), "HRN_BATCH_HELPER=exit")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	start, known := ws.StartID(cmd.Process.Pid)
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if !known && (runtime.GOOS == "windows" || runtime.GOOS == "linux") {
		t.Fatal("cannot read the running helper's start identity")
	}
	return cmd.Process.Pid, start
}

// leaseFile is the lease of slot n of the repository at dir, as a take
// names it: dir resolved as ws.MainCheckout resolves the main checkout, so
// that a temp dir given as a Windows 8.3 short name still matches.
func leaseFile(t *testing.T, dir string, n int) string {
	t.Helper()
	return slotPath(canon(t, dir), n) + ".lease"
}

// quickUnreadableWait makes a task give up soon on a lease that stays
// unreadable, and returns the bound.
func quickUnreadableWait(t *testing.T) time.Duration {
	t.Helper()
	old := unreadableWait
	unreadableWait = 300 * time.Millisecond
	t.Cleanup(func() { unreadableWait = old })
	return unreadableWait
}

// runStuck runs a batch of one task against slots the test filled, and
// returns the task once it gave up, checking it did so after the bound and
// before the context ends, with no slot, no attempt and no branch.
func runStuck(t *testing.T, dir string, bound time.Duration) TaskResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := time.Now()
	res, err := Run(ctx, Spec{Dir: dir, Parallel: 1, Backend: "wb-ok",
		Tasks: []Task{{ID: "stuck", Prompt: "p"}}}, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	tr := res.Tasks[0]
	if tr.Status != StatusError || tr.Slot != 0 || len(tr.Attempts) != 0 || tr.BranchMade {
		t.Fatalf("task = %+v, want an error with no slot, attempt or branch", tr)
	}
	if time.Since(started) < bound {
		t.Errorf("the task gave up after %s, before the %s bound", time.Since(started), bound)
	}
	if !strings.Contains(tr.Error, "hrn ws list") {
		t.Errorf("error = %q, want it to say how to inspect the slots", tr.Error)
	}
	// A live take creates its lease empty before writing it, and a live hrn ws
	// free or takeover holds the .takeover file while the lease may name a dead
	// process, so the advice must tie the removal of either file to no hrn
	// command running on the repository, never to the slot's process alone.
	for _, old := range []string{"no hrn process holds", "process is gone", "works in its slot"} {
		if strings.Contains(tr.Error, old) {
			t.Errorf("error = %q, ties removing a file to the slot's process (%q)", tr.Error, old)
		}
	}
	if !strings.Contains(tr.Error, "A lease that stays unreadable, or a left .takeover file, may be removed only while no hrn command at all runs on this repository") ||
		!strings.Contains(tr.Error, "tasklist | findstr hrn") || !strings.Contains(tr.Error, "pgrep hrn") {
		t.Errorf("error = %q, want either file removed only while no hrn command runs on the repository, with how to check", tr.Error)
	}
	if b := git(t, dir, "branch", "--list", "hrn/stuck"); b != "" {
		t.Errorf("a task that never got a slot leaves no branch: %q", b)
	}
	return tr
}

// A lease that is not JSON is never read by anyone and never freed, so a
// task waiting on it gives up after the bound and names the file, rather
// than wait until the batch is canceled.
func TestATaskGivesUpOnLeasesThatStayUnreadable(t *testing.T) {
	dir := initRepo(t)
	quickPoll(t)
	bound := quickUnreadableWait(t)
	for n := 1; n <= ws.DefaultMax; n++ {
		writeFile(t, leaseFile(t, dir, n), "{not json")
	}
	tr := runStuck(t, dir, bound)
	for n := 1; n <= ws.DefaultMax; n++ {
		if !strings.Contains(tr.Error, leaseFile(t, dir, n)) {
			t.Errorf("error = %q, want it to name %s", tr.Error, leaseFile(t, dir, n))
		}
		if !exists(leaseFile(t, dir, n)) {
			t.Errorf("the unreadable lease of slot %d should be left for the owner", n)
		}
	}
}

// A <n>.lease.takeover file a crash left stops every take from taking the
// stale lease over, so a task waiting on it gives up after the bound and
// names the file to remove.
func TestATaskGivesUpOnALeaseALeftTakeoverFileBlocks(t *testing.T) {
	dir := initRepo(t)
	quickPoll(t)
	bound := quickUnreadableWait(t)
	gone, start := exitedPID(t)
	for n := 1; n <= ws.DefaultMax; n++ {
		data, _ := json.Marshal(ws.Lease{Holder: "batch old task " + strconv.Itoa(n), PID: gone, Start: start, Taken: time.Now()})
		writeFile(t, leaseFile(t, dir, n), string(data))
		writeFile(t, leaseFile(t, dir, n)+".takeover", "")
	}
	tr := runStuck(t, dir, bound)
	for n := 1; n <= ws.DefaultMax; n++ {
		if !strings.Contains(tr.Error, leaseFile(t, dir, n)+".takeover") {
			t.Errorf("error = %q, want it to name %s.takeover", tr.Error, leaseFile(t, dir, n))
		}
		if !exists(leaseFile(t, dir, n)) || !exists(leaseFile(t, dir, n)+".takeover") {
			t.Errorf("the lease and takeover file of slot %d should be left for the owner", n)
		}
	}
}

// An empty lease is one being written, so a task keeps waiting on it; once
// it reads as another batch's the task waits on that batch as usual,
// however long the other slots stay unreadable, and takes the slot when it
// is given back. Slot 1's lease is rewritten while the task is held in its
// first wait event, so its next look is the one that sees the other batch
// however slow the machine: the task's clock on the empty leases runs from
// its first look, and a look before the rewrite would have run it out.
func TestATaskKeepsWaitingOnALeaseBeingWrittenThatBecomesValid(t *testing.T) {
	dir := initRepo(t)
	quickPoll(t)
	bound := quickUnreadableWait(t)
	for n := 1; n <= ws.DefaultMax; n++ {
		writeFile(t, leaseFile(t, dir, n), "")
	}
	// scans counts the task's looks at the leases.
	var scans atomic.Int32
	rangeComputed = func(int) { scans.Add(1) }
	t.Cleanup(func() { rangeComputed = nil })
	var mu sync.Mutex
	var waits []string
	waited := make(chan string, 100)
	// notify runs on the task's goroutine, between one look and the next:
	// a wait event holds the task there until rewritten is closed, which the
	// test does once slot 1's lease is another batch's, and later events
	// pass straight through.
	rewritten := make(chan struct{})
	release := sync.OnceFunc(func() { close(rewritten) })
	t.Cleanup(release)
	notify := func(e Event) {
		if e.Kind == "wait" {
			mu.Lock()
			waits = append(waits, e.Text)
			mu.Unlock()
			waited <- e.Text
			<-rewritten
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var res Result
	done := make(chan error, 1)
	go func() {
		var err error
		res, err = Run(ctx, Spec{Dir: dir, Parallel: 1, Backend: "wb-ok",
			Tasks: []Task{{ID: "patient", Prompt: "p"}}}, notify)
		done <- err
	}()
	awaitWait(t, waited, done, leaseFile(t, dir, 2)+" is being written")
	// The task is held in that event, so no look runs while slot 1's lease
	// is rewritten; the rewrite is still under worktreeMu, which the take
	// scans under, as every change to a lease a take may read is.
	worktreeMu.Lock()
	data, _ := json.Marshal(ws.Lease{Holder: "batch other task x", PID: os.Getpid(), Taken: time.Now()})
	writeFile(t, leaseFile(t, dir, 1), string(data))
	worktreeMu.Unlock()
	release()
	awaitWait(t, waited, done, `taken by "batch other task x"`)
	// The other leases have been unreadable since the first look, before
	// the first wait event: after the bound their clocks have run out, and
	// two more looks mean one completed with them so and did not give up.
	time.Sleep(bound)
	seen := scans.Load() + 2
	for scans.Load() < seen {
		select {
		case err := <-done:
			t.Fatalf("the task ended with %v, %+v while another batch held slot 1", err, res.Tasks)
		case <-time.After(slotPoll):
		}
	}
	worktreeMu.Lock()
	os.Remove(leaseFile(t, dir, 1))
	worktreeMu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	tr := res.Tasks[0]
	if tr.Status != StatusPass || tr.Slot != 1 {
		t.Fatalf("task = %+v, want a pass in slot 1 once the other batch gave it back", tr)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(waits) == 0 || !strings.Contains(waits[0], leaseFile(t, dir, 2)+" is being written") {
		t.Errorf("wait events = %q, want the first to say slot 2's lease is being written and name it", waits)
	}
	for n := 2; n <= ws.DefaultMax; n++ {
		if !exists(leaseFile(t, dir, n)) {
			t.Errorf("the lease being written on slot %d should stay", n)
		}
	}
}

// awaitWait reads wait events until one says text, failing if the take ends
// first.
func awaitWait(t *testing.T, waits <-chan string, done <-chan error, text string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case w := <-waits:
			if strings.Contains(w, text) {
				return
			}
		case err := <-done:
			t.Fatalf("the take ended with %v before it waited saying %q", err, text)
		case <-deadline:
			t.Fatalf("no wait said %q", text)
		}
	}
}

// A look that does not find a lease unreadable ends the wait on it, a
// stretch spent at the batch's bound with no lease scanned included: when
// the lease is unreadable again later, the 30 seconds count from that look,
// not from the first.
func TestATasksWaitOnUnreadableLeasesStartsOverAfterAWaitAtTheBatchsBound(t *testing.T) {
	dir := initRepo(t)
	quickPoll(t)
	bound := quickUnreadableWait(t)
	root := filepath.Clean(filepath.FromSlash(git(t, dir, "rev-parse", "--show-toplevel")))
	base := git(t, dir, "rev-parse", "HEAD")
	// The leases are rewritten under worktreeMu, which the take scans
	// under, so no look sees a rewrite half done.
	broken := func() {
		worktreeMu.Lock()
		defer worktreeMu.Unlock()
		for n := 1; n <= ws.DefaultMax; n++ {
			writeFile(t, leaseFile(t, dir, n), "{not json")
		}
	}
	broken()
	waits := make(chan string, 100)
	notify := func(e Event) {
		if e.Kind == "wait" {
			waits <- e.Text
		}
	}
	// scans counts the looks that scanned the leases once they were
	// unreadable again: the first sets the clocks, so giving up takes
	// another.
	var scans atomic.Int32
	var counting atomic.Bool
	rangeComputed = func(int) {
		if counting.Load() {
			scans.Add(1)
		}
	}
	t.Cleanup(func() { rangeComputed = nil })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := takeSlot(ctx, &Spec{Parallel: 1}, "b1", &keptSlots{}, "t", "hrn/t", root, base, notify, func(bool) error { return nil })
		done <- err
	}()
	awaitWait(t, waits, done, "is unreadable")
	// Every slot becomes one of this batch's own running tasks', so the task
	// waits at its bound and scans no lease.
	worktreeMu.Lock()
	for n := 1; n <= ws.DefaultMax; n++ {
		data, _ := json.Marshal(ws.Lease{Holder: "batch b1 task t" + strconv.Itoa(n), PID: os.Getpid(), Taken: time.Now()})
		writeFile(t, leaseFile(t, dir, n), string(data))
	}
	worktreeMu.Unlock()
	awaitWait(t, waits, done, "this batch holds")
	time.Sleep(bound) // long enough for the first look's clocks to have run out
	again := time.Now()
	counting.Store(true)
	broken()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "could not be read") {
			t.Fatalf("err = %v, want the task to give up on the unreadable leases", err)
		}
		if since := time.Since(again); since < bound {
			t.Errorf("the task gave up %s after the leases were unreadable again, before the %s bound", since, bound)
		}
		if n := scans.Load(); n < 2 {
			t.Errorf("the task gave up after %d look at the leases unreadable again, want the clocks set by one look and run out at a later one", n)
		}
	case <-ctx.Done():
		t.Fatal("the task did not give up on the unreadable leases")
	}
	if b := git(t, dir, "branch", "--list", "hrn/t"); b != "" {
		t.Errorf("a task that never got a slot leaves no branch: %q", b)
	}
}

// A slot whose lease changed hands under the take, in Held but in neither
// Holders nor Unreadable, is a live process's, which gives it back; an
// unreadable lease promises nothing.
func TestSomeoneWillFreeCountsASlotThatChangedHandsUnderTheTake(t *testing.T) {
	if !someoneWillFree(&ws.NoSlotError{Held: 1}, &keptSlots{}) {
		t.Error("a slot that changed hands under the take is held by a live process")
	}
	if someoneWillFree(&ws.NoSlotError{Held: 1, Unreadable: []int{1}}, &keptSlots{}) {
		t.Error("an unreadable lease is no promise that the slot comes free")
	}
}
