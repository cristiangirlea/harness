package ws

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// gitWait bounds one fixture git command, so a stuck one fails its test with
// the command named instead of hanging until the package timeout.
const gitWait = 2 * time.Minute

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), gitWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	// A child git leaves behind (auto gc, a daemon) can hold the output pipe open.
	cmd.WaitDelay = 10 * time.Second
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("git %v in %s still running after %s\n%s", args, dir, gitWait, out)
	}
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// fixture is a bare origin with one commit on main and the main checkout
// whose origin it is, both under one temporary folder. Nothing is pushed:
// origin fetches what a push would send (publish), so the fixture works under
// hrn's guard too, which refuses every push.
type fixture struct {
	t      *testing.T
	root   string
	origin string
	repo   string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, root: root, origin: filepath.Join(root, "origin.git"), repo: filepath.Join(root, "repo")}
	git(t, root, "init", "-q", "-b", "main", f.repo)
	f.identity(f.repo)
	f.write(f.repo, ".gitignore", "target/\n")
	f.write(f.repo, "README.md", "hello\n")
	git(t, f.repo, "add", ".")
	git(t, f.repo, "commit", "-q", "-m", "init")
	git(t, root, "clone", "-q", "--bare", f.repo, f.origin)
	git(t, f.repo, "remote", "add", "origin", f.origin)
	git(t, f.repo, "fetch", "-q", "origin")
	git(t, f.repo, "remote", "set-head", "origin", "main")
	return f
}

// publish puts dir's branch on origin and updates dir's view of origin, as
// `git push origin <branch>` does.
func (f *fixture) publish(dir, branch string) {
	f.t.Helper()
	git(f.t, f.origin, "fetch", "-q", dir, "+refs/heads/"+branch+":refs/heads/"+branch)
	git(f.t, dir, "fetch", "-q", "origin")
}

func (f *fixture) identity(dir string) {
	git(f.t, dir, "config", "user.email", "test@example.com")
	git(f.t, dir, "config", "user.name", "Test")
}

func (f *fixture) write(dir, name, body string) {
	f.t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// commit writes name in dir and commits it.
func (f *fixture) commit(dir, name string) string {
	f.t.Helper()
	f.write(dir, name, name+"\n")
	git(f.t, dir, "add", name)
	git(f.t, dir, "commit", "-q", "-m", name)
	return git(f.t, dir, "rev-parse", "HEAD")
}

// elsewhere commits on branch in another clone and publishes it, as a
// colleague does; the main checkout has not fetched it.
func (f *fixture) elsewhere(branch, name string) string {
	f.t.Helper()
	other := filepath.Join(f.root, "other-"+name)
	git(f.t, f.root, "clone", "-q", f.origin, other)
	f.identity(other)
	if branch != "main" {
		git(f.t, other, "switch", "-q", "-c", branch)
	}
	head := f.commit(other, name)
	f.publish(other, branch)
	return head
}

func (f *fixture) slot(n int) string {
	return filepath.Join(f.repo, ".hrn", "slots", strconv.Itoa(n))
}

func (f *fixture) take(opt TakeOptions) Taken {
	f.t.Helper()
	if opt.Repo == "" {
		opt.Repo = f.repo
	}
	got, err := Take(context.Background(), opt)
	if err != nil {
		f.t.Fatalf("take %+v: %v", opt, err)
	}
	return got
}

// takeWithExitedPID gives a taken slot the identity of a helper that has exited.
func takeWithExitedPID(t *testing.T, f *fixture, opt TakeOptions) Taken {
	t.Helper()
	got := f.take(opt)
	got.Lease.PID, got.Lease.Start = exitedPID(t)
	data, err := json.Marshal(got.Lease)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.repo, ".hrn", "slots", strconv.Itoa(got.N)+".lease"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return got
}

func samePathT(t *testing.T, got, want string) {
	t.Helper()
	g, err1 := filepath.EvalSymlinks(got)
	w, err2 := filepath.EvalSymlinks(want)
	if err1 != nil || err2 != nil || !strings.EqualFold(g, w) {
		t.Fatalf("path %q, want %q", got, want)
	}
}

func readLeaseT(t *testing.T, p string) (Lease, string) {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var l Lease
	if err := json.Unmarshal(data, &l); err != nil {
		t.Fatalf("lease %s: %v\n%s", p, err, data)
	}
	return l, string(data)
}

func TestTakeMakesALockedSlotOnANewBranchFromOriginsDefaultBranch(t *testing.T) {
	f := newFixture(t)
	ahead := f.elsewhere("main", "ahead.txt") // only a fetch sees it
	got := f.take(TakeOptions{Branch: "fix-x", Holder: "alice"})
	samePathT(t, got.Path, f.slot(1))
	if got.N != 1 {
		t.Errorf("slot %d, want 1", got.N)
	}
	if head := git(t, got.Path, "rev-parse", "HEAD"); head != ahead {
		t.Errorf("HEAD %s, want origin's main %s, fetched", head, ahead)
	}
	if b := git(t, got.Path, "branch", "--show-current"); b != "fix-x" {
		t.Errorf("branch %q, want fix-x", b)
	}
	list := git(t, f.repo, "worktree", "list", "--porcelain")
	var block string
	for _, b := range strings.Split(list, "\n\n") {
		if strings.Contains(strings.ToLower(filepath.ToSlash(b)), strings.ToLower(filepath.ToSlash(f.slot(1)))) {
			block = b
		}
	}
	if !strings.Contains(block, "\nlocked") {
		t.Errorf("the slot is not a locked worktree:\n%s", list)
	}
	if data, err := os.ReadFile(filepath.Join(f.repo, ".hrn", ".gitignore")); err != nil || string(data) != "*\n" {
		t.Errorf(".hrn/.gitignore: %q, %v", data, err)
	}
	if st := git(t, f.repo, "status", "--porcelain"); st != "" {
		t.Errorf("the main checkout should stay clean, got:\n%s", st)
	}
	l, raw := readLeaseT(t, filepath.Join(f.repo, ".hrn", "slots", "1.lease"))
	if l.Holder != "alice" || l.Branch != "fix-x" || l.Taken.IsZero() {
		t.Errorf("lease %+v", l)
	}
	if strings.Contains(raw, "pid") || strings.Contains(raw, "start") {
		t.Errorf("a lease without a pid writes neither pid nor start: %s", raw)
	}
}

func TestASecondTakeGetsTheNextSlot(t *testing.T) {
	f := newFixture(t)
	f.take(TakeOptions{Holder: "a"})
	got := f.take(TakeOptions{Holder: "b"})
	samePathT(t, got.Path, f.slot(2))
}

func TestTakeDefaultTriesFiveSlots(t *testing.T) {
	f := newFixture(t)
	for i := 1; i <= 5; i++ {
		got := f.take(TakeOptions{Holder: fmt.Sprintf("holder-%d", i), NoFetch: true})
		if got.N != i {
			t.Fatalf("take %d got slot %d, want %d", i, got.N, i)
		}
	}
	_, err := Take(context.Background(), TakeOptions{Repo: f.repo, Holder: "sixth", NoFetch: true})
	var full *NoSlotError
	if !errors.As(err, &full) || full.Max != 5 {
		t.Fatalf("sixth take: %v, want no slot through 5", err)
	}
	if _, err := os.Stat(f.slot(6)); !os.IsNotExist(err) {
		t.Errorf("slot 6 should not be made: %v", err)
	}
}

func TestTakeRefusesMaxAboveTen(t *testing.T) {
	_, err := Take(context.Background(), TakeOptions{Max: 11})
	if err == nil || !strings.Contains(err.Error(), "at most 10 slots per repository") {
		t.Fatalf("Max 11: %v, want the repository limit", err)
	}
}

func TestSessionsCannotExtendRangePastTen(t *testing.T) {
	f := newFixture(t)
	for n := 1; n <= 10; n++ {
		f.write(f.repo, filepath.Join(".hrn", "slots", fmt.Sprintf("%d.lease", n)),
			fmt.Sprintf(`{"holder":"session","taken":%q}`, time.Now().UTC().Format(time.RFC3339)))
	}
	got, err := RangeBeyondSessions(context.Background(), f.repo, 5)
	if err != nil || got != 10 {
		t.Fatalf("range = %d, %v; want 10", got, err)
	}
	_, err = Take(context.Background(), TakeOptions{Repo: f.repo, Max: got, NoFetch: true})
	var full *NoSlotError
	if !errors.As(err, &full) || !strings.Contains(err.Error(), "at most 10 slots per repository") {
		t.Fatalf("take through range: %v, want the limit named", err)
	}
	if _, err := os.Stat(f.slot(11)); !os.IsNotExist(err) {
		t.Errorf("slot 11 should not be made: %v", err)
	}
}

func TestTakeFromInsideASlotUsesTheMainCheckout(t *testing.T) {
	f := newFixture(t)
	one := f.take(TakeOptions{Holder: "a"})
	got := f.take(TakeOptions{Repo: one.Path, Holder: "b"})
	samePathT(t, got.Path, f.slot(2))
}

func TestTakeAtMaxRefusesAndNamesEveryHolder(t *testing.T) {
	f := newFixture(t)
	f.take(TakeOptions{Holder: "alice", Max: 2})
	f.take(TakeOptions{Holder: "bob", Max: 2})
	_, err := Take(context.Background(), TakeOptions{Repo: f.repo, Holder: "carol", Max: 2})
	if err == nil {
		t.Fatal("a third take with --max 2 should fail")
	}
	for _, want := range []string{"slot 1", "alice", "slot 2", "bob", "since", "hrn ws free", "--max"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
	if _, err := os.Stat(f.slot(3)); err == nil {
		t.Error("no slot 3 should be made past --max")
	}
	var full *NoSlotError
	if !errors.As(err, &full) || full.Max != 2 || full.Held != 2 || len(full.Passed) != 2 ||
		!strings.Contains(full.Passed[0], "alice") || !strings.Contains(full.Passed[1], "bob") {
		t.Errorf("the error should be a *NoSlotError naming both slots, got %#v", err)
	}
	if len(full.Holders) != 2 || full.Holders[0].Holder != "alice" || full.Holders[1].Holder != "bob" {
		t.Errorf("the error should carry both leases, got %+v", full.Holders)
	}
}

func TestNoSlotErrorCountsOnlyHeldSlotsNotSkippedOnes(t *testing.T) {
	f := newFixture(t)
	slot := f.take(TakeOptions{Holder: "a"}).Path
	f.write(slot, "left-behind.txt", "x\n")
	os.Remove(filepath.Join(f.repo, ".hrn", "slots", "1.lease")) // skipped, held by no one
	f.take(TakeOptions{Holder: "b"})                             // slot 2, held
	_, err := Take(context.Background(), TakeOptions{Repo: f.repo, Holder: "c", Max: 2})
	var full *NoSlotError
	if !errors.As(err, &full) || full.Held != 1 || len(full.Passed) != 2 || len(full.Holders) != 1 || full.Holders[0].Holder != "b" {
		t.Errorf("want one held and one skipped slot, got %#v", err)
	}
}

// A lease that cannot be read (being written, or unreadable) is held by
// someone, so it counts in Held, but no Lease says by whom.
func TestNoSlotErrorHoldersLeaveOutALeaseItCannotRead(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.repo, ".hrn", "slots")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "1.lease"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.take(TakeOptions{Holder: "b", Max: 2})
	_, err := Take(context.Background(), TakeOptions{Repo: f.repo, Holder: "c", Max: 2})
	var full *NoSlotError
	if !errors.As(err, &full) || full.Held != 2 || len(full.Holders) != 1 || full.Holders[0].Holder != "b" {
		t.Errorf("want two held slots and one readable lease, got %#v", err)
	}
	if full == nil || !slices.Equal(full.Unreadable, []int{1}) {
		t.Errorf("unreadable slots = %v, want slot 1 alone", full.Unreadable)
	}
}

// A lease being written (empty) and a stale lease a left takeover file
// blocks are unreadable too, and each line names the file in the way.
func TestNoSlotErrorNamesTheFileBehindEachLeaseItCannotRead(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.repo, ".hrn", "slots")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "1.lease"), nil, 0o644); err != nil { // being written
		t.Fatal(err)
	}
	pid, start := exitedPID(t)
	writeLease(t, f.repo, 2, pid, start, "batch y task b")
	if err := os.WriteFile(filepath.Join(dir, "2.lease.takeover"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Take(context.Background(), TakeOptions{Repo: f.repo, Holder: "c", Max: 2})
	var full *NoSlotError
	if !errors.As(err, &full) || full.Held != 2 || len(full.Holders) != 0 || !slices.Equal(full.Unreadable, []int{1, 2}) {
		t.Fatalf("want two held slots, both unreadable, got %#v", err)
	}
	if len(full.Passed) != 2 || !strings.Contains(full.Passed[0], filepath.Join(dir, "1.lease")+" is being written") ||
		!strings.Contains(full.Passed[1], filepath.Join(dir, "2.lease.takeover")) {
		t.Errorf("passed = %q, want the lease being written and the takeover file named", full.Passed)
	}
}

// A stale lease that a live process takes over or frees between a take's
// read of it and its lock on the slot is contention, not a lease that
// cannot be read: the slot is passed over as held, outside Unreadable, and
// the newcomer's lease stays.
func TestNoSlotErrorKeepsALeaseThatChangedUnderTheTakeOutOfUnreadable(t *testing.T) {
	f := newFixture(t)
	pid, start := exitedPID(t)
	writeLease(t, f.repo, 1, pid, start, "batch old task a")
	lp := filepath.Join(f.repo, ".hrn", "slots", "1.lease")
	data, _ := json.Marshal(Lease{Holder: "newcomer", PID: os.Getpid(), Taken: time.Now()})
	beforeTakeoverLock = func() {
		if err := os.WriteFile(lp, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { beforeTakeoverLock = nil }()
	_, err := Take(context.Background(), TakeOptions{Repo: f.repo, Holder: "c", Max: 1})
	var full *NoSlotError
	if !errors.As(err, &full) || full.Held != 1 || len(full.Holders) != 0 {
		t.Fatalf("want one held slot with no lease read, got %#v", err)
	}
	if len(full.Unreadable) != 0 {
		t.Errorf("unreadable slots = %v, want none: a lease that changed hands is not one that cannot be read", full.Unreadable)
	}
	if len(full.Passed) != 1 || !strings.Contains(full.Passed[0], "changed while it was being taken over") {
		t.Errorf("passed = %q, want the slot passed over for its lease changing hands", full.Passed)
	}
	if l, _ := readLeaseT(t, lp); l.Holder != "newcomer" {
		t.Errorf("the newcomer's lease should stay: %+v", l)
	}
}

func TestTakeFailingForAnotherReasonIsNoNoSlotError(t *testing.T) {
	f := newFixture(t)
	_, err := Take(context.Background(), TakeOptions{Repo: f.repo, Branch: "main", Holder: "a"})
	var full *NoSlotError
	if err == nil || errors.As(err, &full) {
		t.Errorf("a branch checked out elsewhere is not a full set of slots: %#v", err)
	}
}

func TestFreeRefusesUnsavedOrUnpushedWorkAndDetachesOnceItIsPushed(t *testing.T) {
	f := newFixture(t)
	slot := f.take(TakeOptions{Branch: "work", Holder: "a"}).Path
	ctx := context.Background()

	f.write(slot, "README.md", "changed\n")
	if _, err := Free(ctx, slot); err == nil || !strings.Contains(err.Error(), "README.md") {
		t.Errorf("free with a tracked change: %v", err)
	}
	git(t, slot, "checkout", "--", "README.md")

	f.write(slot, "notes.txt", "x\n")
	if _, err := Free(ctx, slot); err == nil || !strings.Contains(err.Error(), "notes.txt") {
		t.Errorf("free with an untracked file: %v", err)
	}
	os.Remove(filepath.Join(slot, "notes.txt"))

	f.commit(slot, "work.txt")
	if _, err := Free(ctx, filepath.Join(slot, ".")); err == nil || !strings.Contains(err.Error(), "1 commit") {
		t.Errorf("free with an unpushed commit: %v", err)
	}

	f.publish(slot, "work")
	sub := filepath.Join(slot, "sub")
	os.MkdirAll(sub, 0o755)
	freed, err := Free(ctx, sub)
	if err != nil {
		t.Fatalf("free after the push: %v", err)
	}
	if freed.N != 1 {
		t.Errorf("freed slot %d, want 1", freed.N)
	}
	if b := git(t, slot, "branch", "--show-current"); b != "" {
		t.Errorf("HEAD should be detached, is on %q", b)
	}
	if _, err := os.Stat(filepath.Join(f.repo, ".hrn", "slots", "1.lease")); !os.IsNotExist(err) {
		t.Errorf("the lease should be gone: %v", err)
	}
	if _, err := Free(ctx, slot); err == nil {
		t.Error("freeing a free slot again should be refused")
	}
}

func TestFreeRefusesALeaseHeldByARunningProcess(t *testing.T) {
	f := newFixture(t)
	got := f.take(TakeOptions{Branch: "hrn/t1", Holder: "batch b task t1", PID: os.Getpid()})
	lp := filepath.Join(f.repo, ".hrn", "slots", "1.lease")
	_, before := readLeaseT(t, lp)
	_, err := Free(context.Background(), got.Path)
	if err == nil || !strings.Contains(err.Error(), "running process "+strconv.Itoa(os.Getpid())) {
		t.Errorf("free of a live holder's slot: %v", err)
	}
	if b := git(t, got.Path, "branch", "--show-current"); b != "hrn/t1" {
		t.Errorf("HEAD should stay on hrn/t1, is on %q", b)
	}
	if _, after := readLeaseT(t, lp); after != before {
		t.Errorf("the lease changed:\n%s\nwant\n%s", after, before)
	}
}

func TestFreeGivesBackALeaseWhoseProcessExited(t *testing.T) {
	f := newFixture(t)
	got := takeWithExitedPID(t, f, TakeOptions{Holder: "gone"})
	if _, err := Free(context.Background(), got.Path); err != nil {
		t.Errorf("free of a stale lease: %v", err)
	}
}

func TestFreeGivesBackALeaseWhosePIDWasReused(t *testing.T) {
	id, known := startID(os.Getpid())
	if !known {
		t.Skip("process start identity is unavailable on this platform")
	}
	f := newFixture(t)
	got := f.take(TakeOptions{Holder: "gone", PID: os.Getpid()})
	lp := filepath.Join(f.repo, ".hrn", "slots", "1.lease")
	lease := got.Lease
	lease.Start = strings.SplitN(id, ":", 2)[0] + ":1"
	if lease.Start == id {
		lease.Start += "0"
	}
	raw, err := json.Marshal(lease)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lp, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Free(context.Background(), got.Path); err != nil {
		t.Errorf("free of a lease with a reused pid: %v", err)
	}
}

func TestTakeRecordsProcessStartIdentity(t *testing.T) {
	id, known := startID(os.Getpid())
	if !known {
		t.Skip("process start identity is unavailable on this platform")
	}
	f := newFixture(t)
	got := f.take(TakeOptions{Holder: "batch", PID: os.Getpid()})
	l, _ := readLeaseT(t, filepath.Join(f.repo, ".hrn", "slots", "1.lease"))
	if got.Lease.Start != id || l.Start != id {
		t.Errorf("returned start %q, stored start %q; want %q", got.Lease.Start, l.Start, id)
	}
}

func TestOldLeaseJSONWithoutStartLoads(t *testing.T) {
	var l Lease
	if err := json.Unmarshal([]byte(`{"holder":"old","pid":123,"taken":"2026-10-13T00:00:00Z","branch":""}`), &l); err != nil {
		t.Fatal(err)
	}
	if l.Holder != "old" || l.PID != 123 || l.Start != "" {
		t.Errorf("old lease loaded as %+v", l)
	}
}

// The recovery docs/workspaces.md gives for a slot a batch kept: the work is
// saved on a local branch, the slot detached at a commit origin holds, and
// free then gives it back once hrn has exited.
func TestASlotABatchKeptIsRecoveredAsTheDocsSay(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	got := takeWithExitedPID(t, f, TakeOptions{Branch: "hrn/t1", Holder: "batch b task t1"})
	slot := got.Path
	f.identity(slot)
	git(t, slot, "switch", "-q", "--detach")
	detached := f.commit(slot, "detached.txt")
	f.write(slot, "notes.txt", "x\n")

	rows, err := List(ctx, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].State != "stale" || rows[0].Holder != "batch b task t1" || !strings.Contains(rows[0].Contents, "notes.txt") {
		t.Fatalf("list: %+v", rows)
	}
	if _, err := Free(ctx, slot); err == nil || !strings.Contains(err.Error(), "notes.txt") {
		t.Fatalf("free with an untracked file: %v", err)
	}

	git(t, slot, "add", "notes.txt")
	git(t, slot, "commit", "-q", "-m", "notes")
	git(t, slot, "branch", "keep-t1")
	if _, err := Free(ctx, slot); err == nil || !strings.Contains(err.Error(), "2 commits on no remote") {
		t.Fatalf("free while HEAD holds commits only a local branch has: %v", err)
	}

	git(t, slot, "switch", "-q", "--detach", "origin/main")
	if _, err := Free(ctx, slot); err != nil {
		t.Fatalf("free after detaching at origin/main: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.repo, ".hrn", "slots", "1.lease")); !os.IsNotExist(err) {
		t.Errorf("the lease should be gone: %v", err)
	}
	if h, want := git(t, slot, "rev-parse", "HEAD"), git(t, f.repo, "rev-parse", "origin/main"); h != want {
		t.Errorf("HEAD %s, want origin/main %s", h, want)
	}
	git(t, f.repo, "merge-base", "--is-ancestor", detached, "keep-t1")
	if files := git(t, f.repo, "ls-tree", "--name-only", "keep-t1"); !strings.Contains(files, "detached.txt") || !strings.Contains(files, "notes.txt") {
		t.Errorf("keep-t1 should hold the work, has %q", files)
	}
	again := f.take(TakeOptions{Holder: "next"})
	if again.N != 1 {
		t.Errorf("the next take got slot %d, want the recovered 1", again.N)
	}

	// The detached commits are on keep-t1, not on hrn/t1: the docs merge the
	// branch step 3 made.
	if err := exec.Command("git", "-C", f.repo, "merge-base", "--is-ancestor", detached, "hrn/t1").Run(); err == nil {
		t.Errorf("hrn/t1 should not hold the detached commit %s", detached)
	}
	git(t, f.repo, "merge", "-q", "keep-t1")
	git(t, f.repo, "merge-base", "--is-ancestor", detached, "main")
	if files := git(t, f.repo, "ls-tree", "--name-only", "main"); !strings.Contains(files, "detached.txt") || !strings.Contains(files, "notes.txt") {
		t.Errorf("main should hold the recovered work after the merge, has %q", files)
	}
}

// The same recovery in a repository whose only remote is not called origin:
// a local branch alone does not satisfy free, and detaching at the remote's
// default branch does.
func TestASlotABatchKeptIsRecoveredWhenTheRemoteIsNotCalledOrigin(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	slot := takeWithExitedPID(t, f, TakeOptions{Branch: "hrn/t1", Holder: "batch b task t1"}).Path
	git(t, f.repo, "remote", "rename", "origin", "upstream")
	f.identity(slot)
	git(t, slot, "switch", "-q", "--detach")
	detached := f.commit(slot, "detached.txt")

	git(t, slot, "branch", "keep-t1")
	if _, err := Free(ctx, slot); err == nil || !strings.Contains(err.Error(), "1 commit on no remote") {
		t.Fatalf("free while HEAD holds a commit only a local branch has: %v", err)
	}

	git(t, slot, "switch", "-q", "--detach", "upstream/main")
	if _, err := Free(ctx, slot); err != nil {
		t.Fatalf("free after detaching at upstream/main: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.repo, ".hrn", "slots", "1.lease")); !os.IsNotExist(err) {
		t.Errorf("the lease should be gone: %v", err)
	}
	git(t, f.repo, "merge-base", "--is-ancestor", detached, "keep-t1")
}

// The same recovery when the only remote was added but never fetched: it has
// no remote-tracking branch to detach at until step 4's fetch makes one.
func TestASlotABatchKeptIsRecoveredOnceAnUnfetchedRemoteIsFetched(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	slot := takeWithExitedPID(t, f, TakeOptions{Branch: "hrn/t1", Holder: "batch b task t1"}).Path
	git(t, f.repo, "remote", "remove", "origin")
	git(t, f.repo, "remote", "add", "upstream", f.origin)
	f.identity(slot)
	git(t, slot, "switch", "-q", "--detach")
	detached := f.commit(slot, "detached.txt")

	git(t, slot, "branch", "keep-t1")
	if _, err := Free(ctx, slot); err == nil || !strings.Contains(err.Error(), "on no remote") {
		t.Fatalf("free while no remote-tracking branch holds HEAD: %v", err)
	}
	if r := git(t, slot, "branch", "-r"); r != "" {
		t.Fatalf("the unfetched remote should have no remote-tracking branch, has %q", r)
	}
	if out, err := exec.Command("git", "-C", slot, "switch", "-q", "--detach", "upstream/main").CombinedOutput(); err == nil {
		t.Fatalf("detaching at upstream/main before the fetch should fail:\n%s", out)
	}

	git(t, slot, "fetch", "-q", "upstream")
	git(t, slot, "switch", "-q", "--detach", "upstream/main")
	if _, err := Free(ctx, slot); err != nil {
		t.Fatalf("free after fetching upstream and detaching at upstream/main: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.repo, ".hrn", "slots", "1.lease")); !os.IsNotExist(err) {
		t.Errorf("the lease should be gone: %v", err)
	}
	git(t, f.repo, "merge-base", "--is-ancestor", detached, "keep-t1")
}

// The same recovery in a repository without remotes, as docs/workspaces.md
// gives it: once a local branch holds HEAD, free gives the slot back without
// moving HEAD to another commit.
func TestASlotABatchKeptWithoutARemoteIsFreedOnceABranchHoldsItsWork(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	git(t, f.repo, "remote", "remove", "origin")
	slot := takeWithExitedPID(t, f, TakeOptions{Branch: "hrn/t1", Holder: "batch b task t1"}).Path
	f.identity(slot)
	git(t, slot, "switch", "-q", "--detach")
	detached := f.commit(slot, "detached.txt")
	f.write(slot, "notes.txt", "x\n")
	if _, err := Free(ctx, slot); err == nil || !strings.Contains(err.Error(), "notes.txt") {
		t.Fatalf("free with an untracked file: %v", err)
	}

	git(t, slot, "add", "notes.txt")
	git(t, slot, "commit", "-q", "-m", "notes")
	if _, err := Free(ctx, slot); err == nil || !strings.Contains(err.Error(), "2 commits on no remote") {
		t.Fatalf("free while HEAD holds commits no branch or tag has: %v", err)
	}

	git(t, slot, "branch", "keep-t1")
	head := git(t, slot, "rev-parse", "HEAD")
	if _, err := Free(ctx, slot); err != nil {
		t.Fatalf("free once keep-t1 holds HEAD: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.repo, ".hrn", "slots", "1.lease")); !os.IsNotExist(err) {
		t.Errorf("the lease should be gone: %v", err)
	}
	if h := git(t, slot, "rev-parse", "HEAD"); h != head {
		t.Errorf("HEAD %s, want it left at %s", h, head)
	}
	if b := git(t, slot, "branch", "--show-current"); b != "" {
		t.Errorf("HEAD should be detached, is on %q", b)
	}
	git(t, f.repo, "merge-base", "--is-ancestor", detached, "keep-t1")
	if files := git(t, f.repo, "ls-tree", "--name-only", "keep-t1"); !strings.Contains(files, "detached.txt") || !strings.Contains(files, "notes.txt") {
		t.Errorf("keep-t1 should hold the work, has %q", files)
	}
}

func TestFreeRefusesWhileTheSlotIsBeingTakenOverAndLeavesItAsItWas(t *testing.T) {
	f := newFixture(t)
	slot := f.take(TakeOptions{Branch: "work", Holder: "a"}).Path
	lp := filepath.Join(f.repo, ".hrn", "slots", "1.lease")
	_, before := readLeaseT(t, lp)
	if err := os.WriteFile(lp+".takeover", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Free(context.Background(), slot); err == nil || !strings.Contains(err.Error(), "being taken over or freed") {
		t.Errorf("free under another caller's takeover lock: %v", err)
	}
	if b := git(t, slot, "branch", "--show-current"); b != "work" {
		t.Errorf("HEAD should stay on work, is on %q", b)
	}
	if _, after := readLeaseT(t, lp); after != before {
		t.Errorf("the lease changed:\n%s\nwant\n%s", after, before)
	}
}

func TestFreeRefusesWhenTheLeaseChangesBeforeItLocksTheSlot(t *testing.T) {
	f := newFixture(t)
	slot := f.take(TakeOptions{Branch: "work", Holder: "a"}).Path
	lp := filepath.Join(f.repo, ".hrn", "slots", "1.lease")
	data, _ := json.Marshal(Lease{Holder: "newcomer", Taken: time.Now(), Branch: "work"})
	beforeFreeLock = func() {
		if err := os.WriteFile(lp, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { beforeFreeLock = nil }()
	if _, err := Free(context.Background(), slot); err == nil || !strings.Contains(err.Error(), "lease changed") {
		t.Errorf("free after the lease changed: %v", err)
	}
	if b := git(t, slot, "branch", "--show-current"); b != "work" {
		t.Errorf("HEAD should stay on work, is on %q", b)
	}
	if l, _ := readLeaseT(t, lp); l.Holder != "newcomer" {
		t.Errorf("the newcomer's lease should stay: %+v", l)
	}
}

func TestReleaseRefusesASlotHoldingChangesOrUntrackedFilesAndKeepsTheLease(t *testing.T) {
	f := newFixture(t)
	got := f.take(TakeOptions{Branch: "hrn/t1", Holder: "batch b task t1", PID: os.Getpid()})
	lp := filepath.Join(f.repo, ".hrn", "slots", "1.lease")
	_, before := readLeaseT(t, lp)

	f.write(got.Path, "README.md", "changed\n")
	if err := Release(context.Background(), got, ""); err == nil || !strings.Contains(err.Error(), "README.md") {
		t.Errorf("release with a tracked change: %v", err)
	}
	git(t, got.Path, "checkout", "--", "README.md")
	f.write(got.Path, "left.txt", "x\n")
	if err := Release(context.Background(), got, ""); err == nil || !strings.Contains(err.Error(), "left.txt") {
		t.Errorf("release with an untracked file: %v", err)
	}
	if b := git(t, got.Path, "branch", "--show-current"); b != "hrn/t1" {
		t.Errorf("a refused release must leave HEAD on hrn/t1, is on %q", b)
	}
	if _, after := readLeaseT(t, lp); after != before {
		t.Errorf("a refused release must leave the lease:\n%s\nwant\n%s", after, before)
	}
}

// A release refused under the slot's takeover lock removes the lock, so the
// next release or take is not stopped by it.
func TestAReleaseRefusedUnderTheLockRemovesTheLock(t *testing.T) {
	f := newFixture(t)
	got := f.take(TakeOptions{Branch: "hrn/t1", Holder: "batch b task t1", PID: os.Getpid()})
	lp := filepath.Join(f.repo, ".hrn", "slots", "1.lease")
	f.write(got.Path, "left.txt", "x\n")
	if err := Release(context.Background(), got, ""); err == nil || !strings.Contains(err.Error(), "left.txt") {
		t.Fatalf("release with an untracked file: %v", err)
	}
	if _, err := os.Stat(lp + ".takeover"); !os.IsNotExist(err) {
		t.Errorf("the takeover lock must go with the refused release: %v", err)
	}
	os.Remove(filepath.Join(got.Path, "left.txt"))
	if err := Release(context.Background(), got, ""); err != nil {
		t.Errorf("a release once the slot is clean: %v", err)
	}
}

// detachAt goes to git as an argument, so one that reads as an option is
// refused before anything is touched.
func TestReleaseRefusesADetachPointThatLooksLikeAnOption(t *testing.T) {
	f := newFixture(t)
	got := f.take(TakeOptions{Branch: "hrn/t1", Holder: "batch b task t1", PID: os.Getpid()})
	lp := filepath.Join(f.repo, ".hrn", "slots", "1.lease")
	_, before := readLeaseT(t, lp)
	if err := Release(context.Background(), got, "--orphan=x"); err == nil || !strings.Contains(err.Error(), "not a commit") {
		t.Errorf("release with detachAt %q: %v", "--orphan=x", err)
	}
	if b := git(t, got.Path, "branch", "--show-current"); b != "hrn/t1" {
		t.Errorf("HEAD should stay on hrn/t1, is on %q", b)
	}
	if _, after := readLeaseT(t, lp); after != before {
		t.Errorf("a refused release must leave the lease:\n%s\nwant\n%s", after, before)
	}
}

func TestReleaseRefusesALeaseThatIsNoLongerTheHolders(t *testing.T) {
	f := newFixture(t)
	got := f.take(TakeOptions{Branch: "hrn/t1", Holder: "batch b task t1", PID: os.Getpid()})
	lp := filepath.Join(f.repo, ".hrn", "slots", "1.lease")
	data, _ := json.Marshal(Lease{Holder: "someone else", PID: os.Getpid(), Taken: time.Now(), Branch: "hrn/t1"})
	if err := os.WriteFile(lp, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Release(context.Background(), got, ""); err == nil || !strings.Contains(err.Error(), "someone else") {
		t.Errorf("release of a lease someone else holds now: %v", err)
	}
	if l, _ := readLeaseT(t, lp); l.Holder != "someone else" {
		t.Errorf("the other holder's lease must stay: %+v", l)
	}
	if b := git(t, got.Path, "branch", "--show-current"); b != "hrn/t1" {
		t.Errorf("HEAD should stay on hrn/t1, is on %q", b)
	}
	os.Remove(lp)
	if err := Release(context.Background(), got, ""); err == nil {
		t.Error("release of a slot with no lease should be refused")
	}
}

func TestReleaseRefusesALeaseWithAnotherStartIdentity(t *testing.T) {
	if _, known := startID(os.Getpid()); !known {
		t.Skip("process start identity is unavailable on this platform")
	}
	f := newFixture(t)
	got := f.take(TakeOptions{Holder: "batch", PID: os.Getpid()})
	lp := filepath.Join(f.repo, ".hrn", "slots", "1.lease")
	l := got.Lease
	l.Start += "-other"
	raw, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lp, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Release(context.Background(), got, ""); err == nil {
		t.Error("release of a lease with another start identity should be refused")
	}
	if stored, _ := readLeaseT(t, lp); stored.Start != l.Start {
		t.Errorf("another holder's lease changed: %+v", stored)
	}
}

func TestReleaseRefusesADetachedCommitOnNoRefAndKeepsTheLease(t *testing.T) {
	f := newFixture(t)
	base := git(t, f.repo, "rev-parse", "HEAD")
	got := f.take(TakeOptions{Branch: "hrn/t1", Holder: "batch b task t1", PID: os.Getpid()})
	git(t, got.Path, "switch", "-q", "--detach")
	head := f.commit(got.Path, "detached.txt")
	lp := filepath.Join(f.repo, ".hrn", "slots", "1.lease")
	_, before := readLeaseT(t, lp)
	if err := Release(context.Background(), got, base); err == nil || !strings.Contains(err.Error(), "1 commit on no remote, branch or tag") {
		t.Errorf("release of a detached commit no ref holds: %v", err)
	}
	if h := git(t, got.Path, "rev-parse", "HEAD"); h != head {
		t.Errorf("HEAD moved to %s, want the detached %s", h, head)
	}
	if _, after := readLeaseT(t, lp); after != before {
		t.Errorf("a refused release must leave the lease:\n%s\nwant\n%s", after, before)
	}
}

func TestReleaseOfALeaseThatChangedLeavesHEADWhereItIs(t *testing.T) {
	f := newFixture(t)
	base := git(t, f.repo, "rev-parse", "HEAD")
	got := f.take(TakeOptions{Branch: "hrn/t1", Holder: "batch b task t1", PID: os.Getpid()})
	head := f.commit(got.Path, "work.txt")
	lp := filepath.Join(f.repo, ".hrn", "slots", "1.lease")
	data, _ := json.Marshal(Lease{Holder: "batch c task t1", PID: os.Getpid(), Taken: time.Now(), Branch: "hrn/t1"})
	if err := os.WriteFile(lp, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Release(context.Background(), got, base); err == nil {
		t.Fatal("release of a lease that changed should be refused")
	}
	if b, h := git(t, got.Path, "branch", "--show-current"), git(t, got.Path, "rev-parse", "HEAD"); b != "hrn/t1" || h != head {
		t.Errorf("a refused release must not move HEAD: on %q at %s, want hrn/t1 at %s", b, h, head)
	}
}

func TestReleaseDetachesAtTheCommitItIsGiven(t *testing.T) {
	f := newFixture(t)
	base := git(t, f.repo, "rev-parse", "HEAD")
	got := f.take(TakeOptions{Branch: "hrn/t1", Holder: "batch b task t1", PID: os.Getpid()})
	f.commit(got.Path, "work.txt")
	if err := Release(context.Background(), got, base); err != nil {
		t.Fatalf("release: %v", err)
	}
	if b, h := git(t, got.Path, "branch", "--show-current"), git(t, got.Path, "rev-parse", "HEAD"); b != "" || h != base {
		t.Errorf("HEAD on %q at %s, want detached at %s", b, h, base)
	}
}

func TestReleaseDetachesAndFreesAnUnpushedBranchWithoutAPush(t *testing.T) {
	f := newFixture(t)
	got := f.take(TakeOptions{Branch: "hrn/t1", Holder: "batch b task t1", PID: os.Getpid()})
	head := f.commit(got.Path, "work.txt") // on the local branch only
	f.write(got.Path, "target/x", "build output\n")
	if err := Release(context.Background(), got, ""); err != nil {
		t.Fatalf("release: %v", err)
	}
	if b := git(t, got.Path, "branch", "--show-current"); b != "" {
		t.Errorf("HEAD should be detached, is on %q", b)
	}
	if h := git(t, f.repo, "rev-parse", "hrn/t1"); h != head {
		t.Errorf("the branch should keep its commit: %s, want %s", h, head)
	}
	if _, err := os.Stat(filepath.Join(f.repo, ".hrn", "slots", "1.lease")); !os.IsNotExist(err) {
		t.Errorf("the lease should be gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(got.Path, "target", "x")); err != nil {
		t.Errorf("ignored build output should stay: %v", err)
	}
	if err := Release(context.Background(), got, ""); err == nil {
		t.Error("releasing a released slot again should be refused")
	}
}

func TestTakeAfterFreeReusesTheSlotAndKeepsIgnoredFiles(t *testing.T) {
	f := newFixture(t)
	slot := f.take(TakeOptions{Branch: "a", Holder: "a"}).Path
	f.commit(slot, "a.txt")
	f.write(slot, "target/x", "build output\n")
	f.publish(slot, "a")
	if _, err := Free(context.Background(), slot); err != nil {
		t.Fatal(err)
	}
	got := f.take(TakeOptions{Holder: "b"})
	samePathT(t, got.Path, f.slot(1))
	if _, err := os.Stat(filepath.Join(got.Path, "target", "x")); err != nil {
		t.Errorf("the ignored build output should still be there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(got.Path, "a.txt")); !os.IsNotExist(err) {
		t.Errorf("the previous branch's file should be gone: %v", err)
	}
	if head, main := git(t, got.Path, "rev-parse", "HEAD"), git(t, f.repo, "rev-parse", "refs/remotes/origin/main"); head != main {
		t.Errorf("HEAD %s, want origin/main %s", head, main)
	}
}

func TestTakeSkipsADirtySlotWithoutALeaseAndSaysWhy(t *testing.T) {
	f := newFixture(t)
	slot := f.take(TakeOptions{Holder: "a"}).Path
	f.write(slot, "left-behind.txt", "x\n")
	os.Remove(filepath.Join(f.repo, ".hrn", "slots", "1.lease")) // a lease lost, work left
	got := f.take(TakeOptions{Holder: "b"})
	samePathT(t, got.Path, f.slot(2))
	if !strings.Contains(strings.Join(got.Notes, "\n"), "left-behind.txt") {
		t.Errorf("notes should say why slot 1 was skipped: %q", got.Notes)
	}
	if _, err := os.Stat(filepath.Join(slot, "left-behind.txt")); err != nil {
		t.Errorf("take must not clean someone's files: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.repo, ".hrn", "slots", "1.lease")); !os.IsNotExist(err) {
		t.Errorf("the skipped slot's lease should be removed again: %v", err)
	}
}

func TestTakeSkipsACleanSlotHoldingACommitOnNoRemote(t *testing.T) {
	f := newFixture(t)
	slot := f.take(TakeOptions{Holder: "a"}).Path
	head := f.commit(slot, "crashed.txt")                        // detached, never pushed
	os.Remove(filepath.Join(f.repo, ".hrn", "slots", "1.lease")) // the holder's lease is gone
	got := f.take(TakeOptions{Holder: "b"})
	samePathT(t, got.Path, f.slot(2))
	if !strings.Contains(strings.Join(got.Notes, "\n"), "1 commit on no remote") {
		t.Errorf("notes should say why slot 1 was skipped: %q", got.Notes)
	}
	if h := git(t, slot, "rev-parse", "HEAD"); h != head {
		t.Errorf("slot 1's HEAD moved to %s, want the unpushed %s", h, head)
	}
}

func TestTakeReusesAReleasedSlotWhoseCommitOnlyALocalBranchHolds(t *testing.T) {
	f := newFixture(t)
	got := f.take(TakeOptions{Branch: "hrn/t1", Holder: "batch", PID: os.Getpid()})
	f.commit(got.Path, "work.txt") // on hrn/t1 only, never pushed
	if err := Release(context.Background(), got, ""); err != nil {
		t.Fatal(err)
	}
	again := f.take(TakeOptions{Branch: "hrn/t2", Holder: "batch", PID: os.Getpid()})
	samePathT(t, again.Path, f.slot(1))
	if len(again.Notes) != 0 {
		t.Errorf("nothing was skipped, yet: %q", again.Notes)
	}
}

func TestTakeChecksOutAnExistingLocalBranch(t *testing.T) {
	f := newFixture(t)
	git(t, f.repo, "switch", "-q", "-c", "local")
	head := f.commit(f.repo, "local.txt")
	git(t, f.repo, "switch", "-q", "main")
	got := f.take(TakeOptions{Branch: "local", Holder: "a"})
	if h := git(t, got.Path, "rev-parse", "HEAD"); h != head {
		t.Errorf("HEAD %s, want the local branch's %s", h, head)
	}
}

func TestTakeTracksABranchThatIsOnlyOnOrigin(t *testing.T) {
	f := newFixture(t)
	head := f.elsewhere("theirs", "theirs.txt")
	got := f.take(TakeOptions{Branch: "theirs", Holder: "a"})
	if h := git(t, got.Path, "rev-parse", "HEAD"); h != head {
		t.Errorf("HEAD %s, want origin's theirs %s", h, head)
	}
	if up := git(t, got.Path, "rev-parse", "--abbrev-ref", "@{upstream}"); up != "origin/theirs" {
		t.Errorf("upstream %q, want origin/theirs", up)
	}
}

func TestTakeReportsABranchCheckedOutElsewhereAndReleasesTheLease(t *testing.T) {
	f := newFixture(t)
	_, err := Take(context.Background(), TakeOptions{Repo: f.repo, Branch: "main", Holder: "a"})
	if err == nil || !strings.Contains(err.Error(), "main") {
		t.Fatalf("taking main, checked out in the main checkout, should fail with git's message: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.repo, ".hrn", "slots", "1.lease")); !os.IsNotExist(err) {
		t.Errorf("the lease should be released: %v", err)
	}
}

func TestTakeTakesOverALeaseWhoseProcessExitedButNotALiveOne(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.repo, ".hrn", "slots")
	os.MkdirAll(dir, 0o755)
	writeLease := func(n, pid int, start string) {
		data, _ := json.Marshal(Lease{Holder: "batch", PID: pid, Start: start, Taken: time.Now()})
		if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(n)+".lease"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeLease(1, os.Getpid(), "") // an old lease without Start still uses pid liveness
	pid, start := exitedPID(t)
	writeLease(2, pid, start)
	got := f.take(TakeOptions{Holder: "new", PID: os.Getpid()})
	samePathT(t, got.Path, f.slot(2))
	if l, _ := readLeaseT(t, filepath.Join(dir, "2.lease")); l.Holder != "new" || l.PID != os.Getpid() {
		t.Errorf("slot 2's lease should be the new holder's: %+v", l)
	}
	if l, _ := readLeaseT(t, filepath.Join(dir, "1.lease")); l.Holder != "batch" {
		t.Errorf("a live holder's lease must stay: %+v", l)
	}
}

func TestConcurrentTakesNeverShareASlot(t *testing.T) {
	f := newFixture(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	paths := map[string]int{}
	failed := 0
	for i := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := Take(context.Background(), TakeOptions{Repo: f.repo, Holder: "g" + strconv.Itoa(i), Max: 4, NoFetch: true})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed++
				return
			}
			paths[strings.ToLower(got.Path)]++
		}()
	}
	wg.Wait()
	if len(paths) != 4 || failed != 2 {
		t.Errorf("got %d distinct slots and %d failures, want 4 and 2: %v", len(paths), failed, paths)
	}
	for p, n := range paths {
		if n != 1 {
			t.Errorf("%s handed out %d times", p, n)
		}
	}
}

// halfAddedWorktree makes the admin folder another `git worktree add` leaves
// for a moment: its commondir file created but not yet written. Git commands
// that list the worktrees (add, list, lock) die on it (issue #188).
func (f *fixture) halfAddedWorktree() string {
	f.t.Helper()
	admin := filepath.Join(f.repo, ".git", "worktrees", "half")
	f.write(admin, "gitdir", filepath.ToSlash(filepath.Join(f.root, "half", ".git"))+"\n")
	f.write(admin, "commondir", "")
	return admin
}

// holdCreateLock takes the slots' create lock as another take would, on a
// handle of its own, and returns the function that releases it.
func (f *fixture) holdCreateLock() (unlock func()) {
	f.t.Helper()
	dir := SlotsDir(f.repo)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	unlock, err := lockCreate(context.Background(), dir)
	if err != nil {
		f.t.Fatal(err)
	}
	return unlock
}

func TestTakeWaitsWhileAnotherTakeIsCreatingASlot(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run("existing="+strconv.FormatBool(existing), func(t *testing.T) {
			f := newFixture(t)
			if existing {
				got := f.take(TakeOptions{Holder: "a", NoFetch: true})
				if _, err := Free(context.Background(), got.Path); err != nil {
					t.Fatal(err)
				}
				git(t, f.repo, "worktree", "unlock", got.Path)
			}
			unlock := f.holdCreateLock()
			admin := f.halfAddedWorktree()
			done := make(chan error, 1)
			go func() {
				_, err := Take(context.Background(), TakeOptions{Repo: f.repo, Holder: "b", Max: 1, NoFetch: true})
				done <- err
			}()
			select {
			case err := <-done:
				t.Fatalf("take did not wait for the create lock: %v", err)
			case <-time.After(time.Second):
			}
			// The other take finishes: its worktree is whole, its lock released.
			if err := os.RemoveAll(admin); err != nil {
				t.Fatal(err)
			}
			unlock()
			if err := <-done; err != nil {
				t.Fatalf("take after the lock was released: %v", err)
			}
			if out := git(t, f.repo, "worktree", "list", "--porcelain"); !strings.Contains(out, "locked "+lockReason) {
				t.Errorf("slot is not locked:\n%s", out)
			}
		})
	}
}

func TestCreateLockIsNeverTakenFromAHolderSlowerThanTheWait(t *testing.T) {
	f := newFixture(t)
	defer func(d time.Duration) { createLockWait = d }(createLockWait)
	createLockWait = 200 * time.Millisecond
	unlock := f.holdCreateLock()
	defer unlock()
	// The holder keeps the lock until the second caller's own wait has run out,
	// as a slow add can; the outer bound only turns a hang into a failure.
	got := make(chan error, 1)
	start := time.Now()
	go func() {
		second, err := lockCreate(context.Background(), SlotsDir(f.repo))
		if err == nil {
			second()
		}
		got <- err
	}()
	select {
	case err := <-got:
		if err == nil {
			t.Fatal("a second caller got the create lock while its holder still held it")
		}
		if !strings.Contains(err.Error(), "waited "+createLockWait.String()) {
			t.Fatalf("second caller: %v, want it to give up after its wait", err)
		}
		if waited := time.Since(start); waited < createLockWait {
			t.Fatalf("second caller gave up after %s, before its wait of %s", waited, createLockWait)
		}
	case <-time.After(time.Minute):
		t.Fatal("a second caller kept waiting past its wait for the create lock")
	}
}

// TestHelperHoldCreateLock is a process that takes the create lock in
// HRN_WS_LOCK_DIR, says so, and holds it until it is killed or its stdin
// closes.
func TestHelperHoldCreateLock(t *testing.T) {
	if os.Getenv("HRN_WS_HELPER") != "hold" {
		return
	}
	if _, err := lockCreate(context.Background(), os.Getenv("HRN_WS_LOCK_DIR")); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Println("locked")
	io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func TestCreateLockHeldByAProcessThatDiesIsReleased(t *testing.T) {
	f := newFixture(t)
	dir := SlotsDir(f.repo)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldCreateLock$")
	cmd.Env = append(os.Environ(), "HRN_WS_HELPER=hold", "HRN_WS_LOCK_DIR="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	if line, _ := bufio.NewReader(stdout).ReadString('\n'); strings.TrimSpace(line) != "locked" {
		t.Fatalf("helper did not take the lock: %q", line)
	}

	got := make(chan error, 1)
	go func() {
		unlock, err := lockCreate(context.Background(), dir)
		if err == nil {
			unlock()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("took the create lock a live process holds: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("lock after its holder died: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("still waiting for a create lock whose holder died")
	}
}

func TestTakeIsNotBlockedByALeftoverCreateLockFile(t *testing.T) {
	f := newFixture(t)
	f.write(SlotsDir(f.repo), ".create.lock", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := Take(ctx, TakeOptions{Repo: f.repo, Holder: "a", NoFetch: true}); err != nil {
		t.Fatalf("take beside a create lock file nobody holds: %v", err)
	}
}

func TestTakeWaitingForTheCreateLockStopsWithItsContext(t *testing.T) {
	f := newFixture(t)
	unlock := f.holdCreateLock()
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := Take(ctx, TakeOptions{Repo: f.repo, Holder: "a", NoFetch: true})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("take while the create lock is held: %v, want the context's deadline", err)
	}
	probe, cancelProbe := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelProbe()
	if second, err := lockCreate(probe, SlotsDir(f.repo)); !errors.Is(err, context.DeadlineExceeded) {
		if err == nil {
			second()
		}
		t.Errorf("the holder lost its create lock to a take that stopped waiting: %v", err)
	}
	if _, err := os.Stat(leasePath(SlotsDir(f.repo), 1)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lease left behind: %v", err)
	}
}

// A take whose context ends before the create lock (resolving the base runs git
// under the same context, which can be slow on a loaded machine) reports the
// context, not the error of the git command it stopped (#257).
func TestATakeWhoseContextEndedReportsTheContextWhereverItStopped(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Take(ctx, TakeOptions{Repo: f.repo, Holder: "a", NoFetch: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("take with an ended context: %v, want the context's error", err)
	}
	if _, err := os.Stat(leasePath(SlotsDir(f.repo), 1)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lease left behind: %v", err)
	}
}

func TestFreeOutsideASlotIsRefused(t *testing.T) {
	f := newFixture(t)
	f.take(TakeOptions{Holder: "a"})
	for _, p := range []string{f.repo, filepath.Join(f.repo, ".hrn", "slots"), t.TempDir()} {
		if _, err := Free(context.Background(), p); err == nil || !strings.Contains(err.Error(), "not inside a slot") {
			t.Errorf("free %s: %v, want not inside a slot", p, err)
		}
	}
}

func TestTakeRefusesABareRepository(t *testing.T) {
	f := newFixture(t)
	if _, err := Take(context.Background(), TakeOptions{Repo: f.origin}); err == nil || !strings.Contains(err.Error(), "bare") {
		t.Errorf("take in a bare repository: %v", err)
	}
}

func TestTakeNoFetchWithoutOriginHEADRefusesToGuessTheDefaultBranch(t *testing.T) {
	f := newFixture(t)
	git(t, f.repo, "remote", "set-head", "origin", "-d")
	_, err := Take(context.Background(), TakeOptions{Repo: f.repo, Holder: "a", NoFetch: true})
	if err == nil || !strings.Contains(err.Error(), "not known locally") || !strings.Contains(err.Error(), "--base") {
		t.Fatalf("take --no-fetch without origin/HEAD: %v", err)
	}
	if _, err := os.Stat(f.slot(1)); err == nil {
		t.Error("no slot should be made when the base is unknown")
	}
	got := f.take(TakeOptions{Holder: "a", NoFetch: true, Base: "origin/main"})
	if h, want := git(t, got.Path, "rev-parse", "HEAD"), git(t, f.repo, "rev-parse", "origin/main"); h != want {
		t.Errorf("HEAD %s, want --base origin/main %s", h, want)
	}
}

func TestTakeWithoutAnOriginStartsFromHEAD(t *testing.T) {
	f := newFixture(t)
	git(t, f.repo, "remote", "remove", "origin")
	head := f.commit(f.repo, "local.txt")
	got := f.take(TakeOptions{Holder: "a"})
	if h := git(t, got.Path, "rev-parse", "HEAD"); h != head {
		t.Errorf("HEAD %s, want the main checkout's %s", h, head)
	}
}

func TestListShowsSlotsLeasesAndWhatIsInThem(t *testing.T) {
	f := newFixture(t)
	f.take(TakeOptions{Branch: "fix-x", Holder: "alice"})
	two := f.take(TakeOptions{Holder: "bob"}).Path
	f.write(two, "wip.txt", "x\n")
	os.Remove(filepath.Join(f.repo, ".hrn", "slots", "2.lease"))
	data, _ := json.Marshal(Lease{Holder: "ghost", Taken: time.Now()})
	os.WriteFile(filepath.Join(f.repo, ".hrn", "slots", "3.lease"), data, 0o644)

	got, err := List(context.Background(), f.repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 rows, got %+v", got)
	}
	if s := got[0]; s.N != 1 || s.State != "taken" || s.Holder != "alice" || s.Branch != "fix-x" || s.Contents != "clean" {
		t.Errorf("slot 1: %+v", s)
	}
	if s := got[1]; s.State != "free" || s.Branch != "" || s.Head == "" || !strings.Contains(s.Contents, "wip.txt") {
		t.Errorf("slot 2, a folder without a lease: %+v", s)
	}
	if s := got[2]; s.State != "taken" || s.Holder != "ghost" || s.Folder || s.Contents != "no folder" {
		t.Errorf("slot 3, a lease without a folder: %+v", s)
	}
}

// writeLease writes slot n's lease by hand, as a batch would.
func writeLease(t *testing.T, repo string, n, pid int, start, holder string) {
	t.Helper()
	dir := filepath.Join(repo, ".hrn", "slots")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(Lease{Holder: holder, PID: pid, Start: start, Taken: time.Now()})
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(n)+".lease"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCountLeasesCountsTheReadableLeasesThatMatch(t *testing.T) {
	f := newFixture(t)
	sessions := func(l Lease) bool { return l.PID == 0 }
	if n, err := CountLeases(context.Background(), f.repo, sessions); err != nil || n != 0 {
		t.Fatalf("no slots yet: %d, %v", n, err)
	}
	f.take(TakeOptions{Holder: "alice"})
	f.take(TakeOptions{Holder: "bob"})
	writeLease(t, f.repo, 3, os.Getpid(), "", "batch x task a")
	pid, start := exitedPID(t)
	writeLease(t, f.repo, 4, pid, start, "batch y task b")
	dir := filepath.Join(f.repo, ".hrn", "slots")
	if err := os.WriteFile(filepath.Join(dir, "5.lease"), nil, 0o644); err != nil { // being written
		t.Fatal(err)
	}
	n, err := CountLeases(context.Background(), filepath.Join(dir, "1"), sessions)
	if err != nil || n != 2 {
		t.Errorf("sessions = %d, %v; want alice's and bob's 2, not a live or exited process's, nor one being written", n, err)
	}
	mine := func(l Lease) bool { return l.PID == os.Getpid() && strings.HasPrefix(l.Holder, "batch x ") }
	if n, err := CountLeases(context.Background(), f.repo, mine); err != nil || n != 1 {
		t.Errorf("batch x's = %d, %v; want 1", n, err)
	}
}

// A session's lease (no pid) is held until hrn ws free, so a caller that
// wants n slots of its own must look past the sessions among the first
// slot numbers, and only those: a session further up widens nothing.
func TestRangeBeyondSessionsSkipsOnlySessionsAmongTheFirstSlots(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if n, err := RangeBeyondSessions(ctx, f.repo, 4); err != nil || n != 4 {
		t.Fatalf("no slots yet: %d, %v; want 4", n, err)
	}
	f.take(TakeOptions{Holder: "alice"}) // slot 1, a session
	writeLease(t, f.repo, 2, os.Getpid(), "", "batch x task a")
	if n, err := RangeBeyondSessions(ctx, f.repo, 4); err != nil || n != 5 {
		t.Errorf("one session in the first slots: %d, %v; want 5", n, err)
	}
	writeLease(t, f.repo, 10, 0, "", "far") // a session past the range
	if n, err := RangeBeyondSessions(ctx, f.repo, 4); err != nil || n != 5 {
		t.Errorf("a session on slot 10 widens nothing: %d, %v; want 5", n, err)
	}
	writeLease(t, f.repo, 5, 0, "", "carol")
	if n, err := RangeBeyondSessions(ctx, f.repo, 4); err != nil || n != 6 {
		t.Errorf("a session on the last slot in range: %d, %v; want 6", n, err)
	}
	if n, err := RangeBeyondSessions(ctx, f.repo, 1); err != nil || n != 2 {
		t.Errorf("one slot: %d, %v; want 2", n, err)
	}
}

func TestRangeBeyondSessionsCountsAnUnreadableLeaseAsTaken(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.repo, ".hrn", "slots")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "1.lease"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "2.lease"), nil, 0o644); err != nil { // being written
		t.Fatal(err)
	}
	if n, err := RangeBeyondSessions(context.Background(), f.repo, 2); err != nil || n != 2 {
		t.Errorf("unreadable leases are not sessions: %d, %v; want 2", n, err)
	}
}

func TestRangeBeyondSessionsRefusesLessThanOne(t *testing.T) {
	f := newFixture(t)
	if _, err := RangeBeyondSessions(context.Background(), f.repo, 0); err == nil {
		t.Error("a range of no slots is a mistake")
	}
}
