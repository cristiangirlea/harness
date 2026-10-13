package ws

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
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
	if strings.Contains(raw, "pid") {
		t.Errorf("a lease without a pid writes none: %s", raw)
	}
}

func TestASecondTakeGetsTheNextSlot(t *testing.T) {
	f := newFixture(t)
	f.take(TakeOptions{Holder: "a"})
	got := f.take(TakeOptions{Holder: "b"})
	samePathT(t, got.Path, f.slot(2))
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
	writeLease := func(n, pid int) {
		data, _ := json.Marshal(Lease{Holder: "batch", PID: pid, Taken: time.Now()})
		if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(n)+".lease"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeLease(1, os.Getpid())
	writeLease(2, exitedPID(t))
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
