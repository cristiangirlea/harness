// Package ws reuses git-locked worktrees and build output under O_EXCL leases.
package ws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultMax is Take's default slot count.
const DefaultMax = 5

// Limit caps slots per repository.
const Limit = 10

const lockReason = "hrn ws slot"

var createLockWait = 2 * time.Minute

// Lease records <n>.lease ownership, stale on process exit or proven PID reuse.
// Without a PID it stays held until Free.
type Lease struct {
	Holder string    `json:"holder"`
	PID    int       `json:"pid,omitempty"`
	Start  string    `json:"start,omitempty"` // holder process's start identity, platform-specific, compared for equality; empty in old leases or where unknown
	Taken  time.Time `json:"taken"`
	Branch string    `json:"branch"`
}

// TakeOptions selects a repository, checkout and lease holder.
type TakeOptions struct {
	Repo    string // any directory of the repository (default: the current one)
	Branch  string // check out this branch, or create it from the base; empty detaches at the base
	Base    string // empty: origin's default branch, or HEAD without an origin
	Holder  string // written into the lease
	PID     int    // written into the lease; must name a running process, so its start identity can be read; 0 holds until Free
	Max     int    // slots to try, 1..Max (default DefaultMax, at most Limit)
	NoFetch bool   // use what origin's refs already say
}

// Taken describes a slot taken or freed.
type Taken struct {
	N     int
	Path  string
	Lease Lease
	Notes []string // slots passed over and why, fetches that failed
}

// Status describes a slot folder, lease or both.
type Status struct {
	N        int       `json:"n"`
	Path     string    `json:"path"`
	State    string    `json:"state"` // free, taken or stale
	Holder   string    `json:"holder,omitempty"`
	PID      int       `json:"pid,omitempty"`
	Since    time.Time `json:"since,omitzero"`
	Branch   string    `json:"branch,omitempty"`
	Head     string    `json:"head,omitempty"` // short commit, when detached
	Folder   bool      `json:"folder"`
	Contents string    `json:"contents"` // clean, no folder, or what is in it
}

// MainCheckout finds dir's main checkout, rejecting bare repositories.
func MainCheckout(ctx context.Context, dir string) (string, error) {
	common, err := gitOut(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("%s is not in a git repository: %w", dir, err)
	}
	bare, err := gitOut(ctx, common, "rev-parse", "--is-bare-repository")
	if err != nil {
		return "", err
	}
	if bare == "true" {
		return "", fmt.Errorf("%s is a bare repository, which has no checkout to keep slots in", common)
	}
	main := filepath.Dir(filepath.FromSlash(common))
	if r, err := filepath.EvalSymlinks(main); err == nil {
		main = r
	}
	return main, nil
}

// SlotsDir locates main's slots and leases.
func SlotsDir(main string) string { return filepath.Join(main, ".hrn", "slots") }

func leasePath(dir string, n int) string { return filepath.Join(dir, strconv.Itoa(n)+".lease") }

// Take leases and switches a slot, preserving ignored files and skipping changes or stranded commits.
// Errors preserve cancellation to distinguish interrupted git from repository damage.
func Take(ctx context.Context, opt TakeOptions) (Taken, error) {
	t, err := take(ctx, opt)
	if err != nil && ctx.Err() != nil && !errors.Is(err, ctx.Err()) {
		err = fmt.Errorf("%w: %w", ctx.Err(), err)
	}
	return t, err
}

func take(ctx context.Context, opt TakeOptions) (Taken, error) {
	if opt.Repo == "" {
		opt.Repo = "."
	}
	if opt.Max <= 0 {
		opt.Max = DefaultMax
	}
	if opt.Max > Limit {
		return Taken{}, fmt.Errorf("--max %d: at most %d slots per repository", opt.Max, Limit)
	}
	for _, v := range []string{opt.Branch, opt.Base} {
		if strings.HasPrefix(v, "-") {
			return Taken{}, fmt.Errorf("%q is not a branch or ref", v)
		}
	}
	main, err := MainCheckout(ctx, opt.Repo)
	if err != nil {
		return Taken{}, err
	}
	if opt.Branch != "" {
		if _, err := gitOut(ctx, main, "check-ref-format", "--branch", opt.Branch); err != nil {
			return Taken{}, fmt.Errorf("%q is not a valid branch name", opt.Branch)
		}
	}
	var notes []string
	base, err := resolveBase(ctx, main, opt, &notes)
	if err != nil {
		return Taken{}, err
	}
	switchArgs, err := planSwitch(ctx, main, base, opt, &notes)
	if err != nil {
		return Taken{}, err
	}

	dir := SlotsDir(main)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Taken{}, err
	}
	if _, err := os.Stat(filepath.Join(main, ".hrn", ".gitignore")); err != nil {
		_ = os.WriteFile(filepath.Join(main, ".hrn", ".gitignore"), []byte("*\n"), 0o644)
	}

	var passed []string
	var holders []Lease
	var unreadable []int
	held := 0
	for n := 1; n <= opt.Max; n++ {
		path := filepath.Join(dir, strconv.Itoa(n))
		lp := leasePath(dir, n)
		lease := Lease{Holder: opt.Holder, PID: opt.PID, Taken: time.Now().UTC().Truncate(time.Second), Branch: opt.Branch}
		if opt.PID != 0 {
			lease.Start, _ = startID(opt.PID)
		}
		busy, holder, changed, err := acquire(lp, lease)
		if err != nil {
			return Taken{Notes: notes}, fmt.Errorf("slot %d: %w", n, err)
		}
		if busy != "" {
			passed = append(passed, fmt.Sprintf("slot %d %s: %s", n, path, busy))
			held++
			switch {
			case holder != nil:
				holders = append(holders, *holder)
			case !changed:
				unreadable = append(unreadable, n)
			}
			continue
		}
		skip, err := prepare(ctx, main, path, base)
		if err != nil || skip != "" {
			removeOwn(lp)
			if err != nil {
				return Taken{Notes: notes}, fmt.Errorf("slot %d: %w", n, err)
			}
			line := fmt.Sprintf("slot %d %s: skipped, %s", n, path, skip)
			passed = append(passed, line)
			notes = append(notes, line)
			continue
		}
		if _, err := gitOut(ctx, path, switchArgs...); err != nil {
			removeOwn(lp)
			return Taken{Notes: notes}, fmt.Errorf("slot %d: %w", n, err)
		}
		return Taken{N: n, Path: path, Lease: lease, Notes: notes}, nil
	}
	return Taken{Notes: notes}, &NoSlotError{Max: opt.Max, Held: held, Holders: holders, Passed: passed, Unreadable: unreadable}
}

// NoSlotError allows waiting when all slots are held or skipped.
type NoSlotError struct {
	Max     int
	Held    int      // how many were passed over for a lease someone holds; the rest were skipped
	Holders []Lease  // the leases of those, in slot order, less any that could not be read
	Passed  []string // one line per slot: its path and who holds it, or why it was skipped
	// Unreadable lists held slots with unreadable leases or blocked takeovers, excluding changed hands.
	Unreadable []int
}

func (e *NoSlotError) Error() string {
	advice := "free one with `hrn ws free <path>`, or allow more with --max"
	if e.Max == Limit {
		advice = fmt.Sprintf("free one with `hrn ws free <path>`; at most %d slots per repository", Limit)
	}
	return fmt.Sprintf("every slot up to --max %d is taken or skipped:\n  %s\n%s",
		e.Max, strings.Join(e.Passed, "\n  "), advice)
}

func resolveBase(ctx context.Context, main string, opt TakeOptions, notes *[]string) (string, error) {
	if opt.Base != "" {
		sha, err := commitOf(ctx, main, opt.Base)
		if err != nil {
			return "", fmt.Errorf("--base %s is not a commit here", opt.Base)
		}
		return sha, nil
	}
	if !hasOrigin(ctx, main) {
		sha, err := commitOf(ctx, main, "HEAD")
		if err != nil {
			return "", fmt.Errorf("the repository has no origin and no commit to start from")
		}
		return sha, nil
	}
	if opt.NoFetch {
		// Require origin/HEAD; guessing main or master can pick the wrong branch.
		if sha, err := commitOf(ctx, main, "refs/remotes/origin/HEAD"); err == nil {
			return sha, nil
		}
		return "", errors.New("origin's default branch is not known locally (no refs/remotes/origin/HEAD); drop --no-fetch or give --base")
	}
	out, err := gitOut(ctx, main, "ls-remote", "--symref", "origin", "HEAD")
	if err != nil {
		return "", fmt.Errorf("asking origin for its default branch (--no-fetch uses what is already fetched): %w", err)
	}
	var name string
	for _, line := range strings.Split(out, "\n") {
		if ref, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
			name, _, _ = strings.Cut(ref, "\t")
		}
	}
	if name == "" {
		return "", errors.New("origin does not say which branch is its default; give --base")
	}
	return fetchRemote(ctx, main, name, notes)
}

// Report cached-ref fallback on fetch failure because it may be stale.
func fetchRemote(ctx context.Context, main, name string, notes *[]string) (string, error) {
	ref := "refs/remotes/origin/" + name
	_, ferr := gitOut(ctx, main, "fetch", "-q", "origin", "+refs/heads/"+name+":"+ref)
	sha, err := commitOf(ctx, main, ref)
	if err != nil {
		if ferr != nil {
			return "", ferr
		}
		return "", err
	}
	if ferr != nil {
		*notes = append(*notes, fmt.Sprintf("fetching origin's %s failed, so the commit already fetched is used: %v", name, ferr))
	}
	return sha, nil
}

func planSwitch(ctx context.Context, main, base string, opt TakeOptions, notes *[]string) ([]string, error) {
	b := opt.Branch
	if b == "" {
		return []string{"switch", "--detach", base}, nil
	}
	if _, err := commitOf(ctx, main, "refs/heads/"+b); err == nil {
		return []string{"switch", b}, nil
	}
	if hasOrigin(ctx, main) {
		remote := "refs/remotes/origin/" + b
		if !opt.NoFetch {
			out, err := gitOut(ctx, main, "ls-remote", "--heads", "origin", "refs/heads/"+b)
			if err != nil {
				return nil, fmt.Errorf("asking origin for branch %s (--no-fetch uses what is already fetched): %w", b, err)
			}
			if out != "" {
				if _, err := fetchRemote(ctx, main, b, notes); err != nil {
					return nil, err
				}
			}
		}
		if _, err := commitOf(ctx, main, remote); err == nil {
			return []string{"switch", "--track", "-c", b, remote}, nil
		}
	}
	return []string{"switch", "-c", b, base}, nil
}

func hasOrigin(ctx context.Context, main string) bool {
	_, err := gitOut(ctx, main, "remote", "get-url", "origin")
	return err == nil
}

func commitOf(ctx context.Context, dir, ref string) (string, error) {
	return gitOut(ctx, dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
}

var beforeTakeoverLock func()

// O_EXCL prevents shared ownership; replace only proven stale leases under the takeover lock.
// changed distinguishes handover contention from unreadable leases.
func acquire(p string, l Lease) (busy string, holder *Lease, changed bool, err error) {
	data, err := json.Marshal(l)
	if err != nil {
		return "", nil, false, err
	}
	for range 2 {
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			_, werr := f.Write(append(data, '\n'))
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			if werr != nil {
				removeOwn(p)
				return "", nil, false, werr
			}
			return "", nil, false, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", nil, false, err
		}
		old, raw, err := readLease(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue // freed in between
		case err != nil:
			return err.Error(), nil, false, nil
		case !old.stale():
			return describe(old), &old, false, nil
		}
		if beforeTakeoverLock != nil {
			beforeTakeoverLock()
		}
		if why, changed := removeLease(p, raw); why != "" {
			return why, nil, changed, nil
		}
	}
	return leaseChanged, nil, true, nil
}

const leaseChanged = "its lease changed while it was being taken over"

// Treat empty leases as held: O_EXCL creation precedes writing.
func readLease(p string) (Lease, []byte, error) {
	raw, err := os.ReadFile(p)
	if err != nil {
		return Lease{}, nil, err
	}
	var l Lease
	if len(bytes.TrimSpace(raw)) == 0 {
		return Lease{}, raw, fmt.Errorf("its lease %s is being written", p)
	}
	if err := json.Unmarshal(raw, &l); err != nil {
		return Lease{}, raw, fmt.Errorf("its lease %s is unreadable: %v", p, err)
	}
	return l, raw, nil
}

func (l Lease) processHolds() bool {
	if l.PID == 0 || !alive(l.PID) {
		return false
	}
	if l.Start != "" {
		// Shared Windows/WSL slots require same-platform start ids to prove PID reuse.
		if id, known := startID(l.PID); known && samePlatform(id, l.Start) && id != l.Start {
			return false
		}
	}
	return true
}

// StartID returns a process's platform-specific start identity if known.
func StartID(pid int) (string, bool) { return startID(pid) }

func samePlatform(a, b string) bool {
	pa, _, _ := strings.Cut(a, ":")
	pb, _, _ := strings.Cut(b, ":")
	return pa == pb
}

func (l Lease) stale() bool { return l.PID != 0 && !l.processHolds() }

func describe(l Lease) string {
	s := fmt.Sprintf("taken by %q since %s", l.Holder, l.Taken.Local().Format("2006-01-02 15:04"))
	if l.PID != 0 {
		s += fmt.Sprintf(" (pid %d)", l.PID)
	}
	if l.Branch != "" {
		s += " on " + l.Branch
	}
	return s
}

// Recheck bytes under lock to avoid deleting a successor's lease.
func removeLease(p string, want []byte) (why string, changed bool) {
	unlock, why := lockLease(p)
	if why != "" {
		return why, false
	}
	defer unlock()
	now, err := os.ReadFile(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", false
	case err != nil:
		return err.Error(), false
	case !bytes.Equal(now, want):
		return leaseChanged, true
	}
	if err := removeOwn(p); err != nil {
		return err.Error(), false
	}
	return "", false
}

// O_EXCL serializes takeover/free; remove leftover locks only with all hrn commands stopped.
func lockLease(p string) (unlock func(), why string) {
	lock := p + ".takeover"
	f, err := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, "its lease is being taken over or freed (if no one is, a crash left " + lock + ", to remove only while no hrn command runs on this repository)"
		}
		return nil, err.Error()
	}
	f.Close()
	return func() { os.Remove(lock) }, ""
}

// Windows readers can briefly deny deletion; retry owned-lease removal.
func removeOwn(p string) error {
	var err error
	for i := 0; i < 10; i++ {
		if err = os.Remove(p); err == nil || errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if runtime.GOOS != "windows" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return err
}

// prepare runs with the caller holding the lease; it creates and locks the worktree on first use, else says why a switch would lose work.
func prepare(ctx context.Context, main, path, base string) (skip string, err error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		unlock, err := lockCreate(ctx, filepath.Dir(path))
		if err != nil {
			return "", err
		}
		defer unlock()
		if _, err := gitOut(ctx, main, "worktree", "add", "--detach", path, base); err != nil {
			return err.Error(), nil
		}
		_, err = gitOut(ctx, main, "worktree", "lock", "--reason", lockReason, path)
		return "", err
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "it is not a folder", nil
	}
	if skip, err := lockExisting(ctx, main, path); err != nil || skip != "" {
		return skip, err
	}
	lines, err := changes(ctx, path)
	if err != nil {
		return "", err
	}
	if len(lines) > 0 {
		return "it holds changes or untracked files: " + firstFew(lines), nil
	}
	count, err := stranded(ctx, path)
	if err != nil {
		return "", err
	}
	if count > 0 {
		word := "commits"
		if count == 1 {
			word = "commit"
		}
		return fmt.Sprintf("it holds %d %s on no remote, branch or tag", count, word), nil
	}
	return "", nil
}

// Lock listing and locking against adds, which expose incomplete git admin directories.
func lockExisting(ctx context.Context, main, path string) (skip string, err error) {
	unlock, err := lockCreate(ctx, filepath.Dir(path))
	if err != nil {
		return "", err
	}
	defer unlock()
	wt, ok, err := worktreeAt(ctx, main, path)
	if err != nil {
		return "", err
	}
	if !ok {
		return "it is not a worktree of this repository", nil
	}
	if !wt.locked {
		if _, err := gitOut(ctx, main, "worktree", "lock", "--reason", lockReason, path); err != nil {
			return "", err
		}
	}
	return "", nil
}

// Lease first, then serialize git add/list/lock to hide incomplete admin directories.
// Never remove this file: OS ownership releases on close or death.
func lockCreate(ctx context.Context, dir string) (unlock func(), err error) {
	p := filepath.Join(dir, ".create.lock")
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(createLockWait)
	delay := 10 * time.Millisecond
	for {
		ok, err := tryLock(f)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("locking %s: %w", p, err)
		}
		if ok {
			return func() {
				unlockFile(f)
				f.Close()
			}, nil
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("waited %s for %s, which other takes kept holding", createLockWait, p)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, 200*time.Millisecond)
	}
}

// TryLock creates p and tries an exclusive lock, returning ok false if held.
// The OS releases ownership on close or death.
func TryLock(p string) (unlock func(), ok bool, err error) {
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, false, err
	}
	if ok, err = tryLock(f); !ok || err != nil {
		f.Close()
		return nil, false, err
	}
	return func() {
		unlockFile(f)
		f.Close()
	}, true, nil
}

// Local branches and tags preserve commits across switches without a push.
func stranded(ctx context.Context, dir string) (int, error) {
	out, err := gitOut(ctx, dir, "rev-list", "--count", "HEAD", "--not", "--remotes", "--branches", "--tags")
	if err != nil {
		if unbornHEAD(ctx, dir) {
			return 0, nil
		}
		return 0, err
	}
	return strconv.Atoi(out)
}

// A quiet missing HEAD has no commits to strand; other git failures keep the slot.
func unbornHEAD(ctx context.Context, dir string) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	_, err := gitCommand(ctx, dir, "rev-parse", "-q", "--verify", "HEAD").Output()
	var exit *exec.ExitError
	// On Windows a git killed by the context also exits 1 with no stderr.
	return ctx.Err() == nil && errors.As(err, &exit) && exit.ExitCode() == 1 && len(exit.Stderr) == 0
}

type worktree struct {
	path   string
	locked bool
}

func worktreeAt(ctx context.Context, main, path string) (worktree, bool, error) {
	out, err := gitStdout(ctx, main, "worktree", "list", "--porcelain")
	if err != nil {
		return worktree{}, false, err
	}
	want := samePath(path)
	for _, block := range strings.Split(out, "\n\n") {
		var w worktree
		for _, line := range strings.Split(block, "\n") {
			if p, ok := strings.CutPrefix(line, "worktree "); ok {
				w.path = p
			}
			if line == "locked" || strings.HasPrefix(line, "locked ") {
				w.locked = true
			}
		}
		if w.path != "" && samePath(w.path) == want {
			return w, true, nil
		}
	}
	return worktree{}, false, nil
}

func changes(ctx context.Context, dir string) ([]string, error) {
	out, err := gitStdout(ctx, dir, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	return lines, nil
}

func firstFew(lines []string) string {
	const few = 3
	if len(lines) <= few {
		return strings.Join(lines, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(lines[:few], ", "), len(lines)-few)
}

// Require a worktree root so git cannot act on the enclosing main checkout.
func isWorktreeRoot(ctx context.Context, dir string) bool {
	top, err := gitOut(ctx, dir, "rev-parse", "--show-toplevel")
	return err == nil && samePath(top) == samePath(dir)
}

func slotOf(main, path string) (int, string, bool) {
	dir := samePath(SlotsDir(main))
	rel, err := filepath.Rel(dir, samePath(path))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return 0, "", false
	}
	first, _, _ := strings.Cut(rel, string(filepath.Separator))
	n, err := strconv.Atoi(first)
	if err != nil || n < 1 || strconv.Itoa(n) != first {
		return 0, "", false
	}
	return n, filepath.Join(SlotsDir(main), first), true
}

var beforeFreeLock func()

// Free releases path's slot unless held by a live process, changed or unpushed.
// Detaching HEAD frees its branch for other checkouts and gc.
func Free(ctx context.Context, path string) (Taken, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Taken{}, err
	}
	notSlot := fmt.Errorf("%s is not inside a slot (<main checkout>/.hrn/slots/<n>)", path)
	main, err := MainCheckout(ctx, abs)
	if err != nil {
		return Taken{}, fmt.Errorf("%w: %v", notSlot, err)
	}
	n, slot, ok := slotOf(main, abs)
	if !ok {
		return Taken{}, notSlot
	}
	lp := leasePath(SlotsDir(main), n)
	lease, raw, err := readLease(lp)
	if errors.Is(err, fs.ErrNotExist) {
		return Taken{}, fmt.Errorf("slot %d has no lease: it is free already", n)
	}
	if err != nil {
		return Taken{}, fmt.Errorf("slot %d: %v", n, err)
	}
	if lease.processHolds() {
		return Taken{}, fmt.Errorf("slot %d is held by running process %d; it gives the slot back itself", n, lease.PID)
	}
	if beforeFreeLock != nil {
		beforeFreeLock()
	}
	// Lock through lease removal to avoid detaching a new holder's branch.
	unlock, why := lockLease(lp)
	if why != "" {
		return Taken{}, fmt.Errorf("slot %d: %s", n, why)
	}
	defer unlock()
	if now, err := os.ReadFile(lp); err != nil || !bytes.Equal(now, raw) {
		return Taken{}, fmt.Errorf("slot %d: its lease changed while it was being freed", n)
	}
	if !isWorktreeRoot(ctx, slot) {
		return Taken{}, fmt.Errorf("slot %d (%s) is not a worktree of this repository", n, slot)
	}
	lines, err := changes(ctx, slot)
	if err != nil {
		return Taken{}, err
	}
	if len(lines) > 0 {
		return Taken{}, fmt.Errorf("slot %d holds changes or untracked files, so it stays taken: %s; commit and push them, or remove them",
			n, firstFew(lines))
	}
	count, err := unpushed(ctx, slot)
	if err != nil {
		return Taken{}, err
	}
	if count > 0 {
		word := "commits"
		if count == 1 {
			word = "commit"
		}
		return Taken{}, fmt.Errorf("slot %d has %d %s on no remote, so it stays taken; push them first", n, count, word)
	}
	detach := []string{"switch", "--detach"}
	if unbornHEAD(ctx, slot) {
		base, err := commitOf(ctx, main, "HEAD")
		if err != nil {
			return Taken{}, err
		}
		detach = append(detach, base)
	}
	if _, err := gitOut(ctx, slot, detach...); err != nil {
		return Taken{}, err
	}
	if err := removeOwn(lp); err != nil {
		return Taken{}, fmt.Errorf("slot %d: %v", n, err)
	}
	return Taken{N: n, Path: slot, Lease: lease}, nil
}

// Release detaches t's clean slot at detachAt (empty: HEAD), removing only its exact lease under lock.
// Local branches preserve unpushed batch results; stranded commits prevent release.
func Release(ctx context.Context, t Taken, detachAt string) error {
	if strings.HasPrefix(detachAt, "-") {
		return fmt.Errorf("%q is not a commit", detachAt)
	}
	main, err := MainCheckout(ctx, t.Path)
	if err != nil {
		return err
	}
	n, slot, ok := slotOf(main, t.Path)
	if !ok || n != t.N {
		return fmt.Errorf("%s is not slot %d of %s", t.Path, t.N, main)
	}
	lp := leasePath(SlotsDir(main), n)
	lease, raw, err := readLease(lp)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("slot %d has no lease: it was given back already", n)
	}
	if err != nil {
		return fmt.Errorf("slot %d: %v", n, err)
	}
	if lease.Holder != t.Lease.Holder || lease.PID != t.Lease.PID || lease.Start != t.Lease.Start || lease.Branch != t.Lease.Branch || !lease.Taken.Equal(t.Lease.Taken) {
		return fmt.Errorf("slot %d is %s now, not %q's, so it stays as it is", n, describe(lease), t.Lease.Holder)
	}
	unlock, why := lockLease(lp)
	if why != "" {
		return fmt.Errorf("slot %d: %s", n, why)
	}
	defer unlock()
	if now, err := os.ReadFile(lp); err != nil || !bytes.Equal(now, raw) {
		return fmt.Errorf("slot %d: its lease changed while it was being given back", n)
	}
	if !isWorktreeRoot(ctx, slot) {
		return fmt.Errorf("slot %d (%s) is not a worktree of this repository", n, slot)
	}
	lines, err := changes(ctx, slot)
	if err != nil {
		return err
	}
	if len(lines) > 0 {
		return fmt.Errorf("slot %d holds changes or untracked files, so it stays taken: %s", n, firstFew(lines))
	}
	count, err := stranded(ctx, slot)
	if err != nil {
		return err
	}
	if count > 0 {
		word := "commits"
		if count == 1 {
			word = "commit"
		}
		return fmt.Errorf("slot %d holds %d %s on no remote, branch or tag, so it stays taken", n, count, word)
	}
	detach := []string{"switch", "--detach"}
	if detachAt != "" {
		detach = append(detach, detachAt)
	}
	if _, err := gitOut(ctx, slot, detach...); err != nil {
		return err
	}
	if err := removeOwn(lp); err != nil {
		return fmt.Errorf("slot %d: %v", n, err)
	}
	return nil
}

// Without remotes, branches and tags suffice to preserve commits.
func unpushed(ctx context.Context, dir string) (int, error) {
	remotes, err := gitOut(ctx, dir, "remote")
	if err != nil {
		return 0, err
	}
	args := []string{"rev-list", "--count", "HEAD", "--not"}
	if remotes != "" {
		args = append(args, "--remotes")
	} else {
		args = append(args, "--branches", "--tags")
	}
	out, err := gitOut(ctx, dir, args...)
	if err != nil {
		if unbornHEAD(ctx, dir) {
			return 0, nil
		}
		return 0, err
	}
	return strconv.Atoi(out)
}

// CountLeases counts matching readable leases, excluding incomplete files.
func CountLeases(ctx context.Context, repo string, match func(Lease) bool) (int, error) {
	main, err := MainCheckout(ctx, repo)
	if err != nil {
		return 0, err
	}
	dir := SlotsDir(main)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".lease")
		if !ok || e.IsDir() {
			continue
		}
		if i, err := strconv.Atoi(name); err != nil || i < 1 || strconv.Itoa(i) != name {
			continue
		}
		if l, _, err := readLease(filepath.Join(dir, e.Name())); err == nil && match(l) {
			n++
		}
	}
	return n, nil
}

// RangeBeyondSessions returns Take's Max for n non-session slots, capped at Limit.
// Skip PID-free sessions held until Free; count process-held and unreadable slots.
func RangeBeyondSessions(ctx context.Context, repo string, n int) (int, error) {
	if n < 1 {
		return 0, fmt.Errorf("a range of %d slots is none", n)
	}
	if n > Limit {
		return 0, fmt.Errorf("a range of %d slots exceeds at most %d slots per repository", n, Limit)
	}
	main, err := MainCheckout(ctx, repo)
	if err != nil {
		return 0, err
	}
	dir := SlotsDir(main)
	counted := 0
	for slot := 1; slot <= Limit; slot++ {
		if l, _, err := readLease(leasePath(dir, slot)); err == nil && l.PID == 0 {
			continue
		}
		if counted++; counted == n {
			return slot, nil
		}
	}
	return Limit, nil
}

// List includes orphan leases and unleased slot folders.
func List(ctx context.Context, repo string) ([]Status, error) {
	main, err := MainCheckout(ctx, repo)
	if err != nil {
		return nil, err
	}
	dir := SlotsDir(main)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".lease")
		if e.IsDir() == (name != e.Name()) {
			continue // a folder named n.lease, or a file that is no lease
		}
		if n, err := strconv.Atoi(name); err == nil && n >= 1 && strconv.Itoa(n) == name {
			seen[n] = true
		}
	}
	var ns []int
	for n := range seen {
		ns = append(ns, n)
	}
	sort.Ints(ns)

	var out []Status
	for _, n := range ns {
		s := Status{N: n, Path: filepath.Join(dir, strconv.Itoa(n)), State: "free"}
		l, _, err := readLease(leasePath(dir, n))
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			s.State, s.Holder = "taken", "?"
		default:
			s.State, s.Holder, s.PID, s.Since = "taken", l.Holder, l.PID, l.Taken
			if l.stale() {
				s.State = "stale"
			}
		}
		if info, err := os.Stat(s.Path); err != nil || !info.IsDir() {
			s.Contents = "no folder"
			out = append(out, s)
			continue
		}
		s.Folder = true
		if !isWorktreeRoot(ctx, s.Path) {
			s.Contents = "not a worktree"
			out = append(out, s)
			continue
		}
		if b, err := gitOut(ctx, s.Path, "symbolic-ref", "--short", "-q", "HEAD"); err == nil {
			s.Branch = b
		} else if h, err := gitOut(ctx, s.Path, "rev-parse", "--short", "HEAD"); err == nil {
			s.Head = h
		}
		lines, err := changes(ctx, s.Path)
		switch {
		case err != nil:
			s.Contents = err.Error()
		case len(lines) == 0:
			s.Contents = "clean"
		default:
			s.Contents = fmt.Sprintf("%d changed or untracked: %s", len(lines), firstFew(lines))
		}
		out = append(out, s)
	}
	return out, nil
}
