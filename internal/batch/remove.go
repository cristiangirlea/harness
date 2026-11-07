package batch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/gitenv"
	"github.com/cristiangirlea/harness/internal/nowindow"
)

// Remove deletes owned branches and worktrees, preserving uncommitted work and slots.
func Remove(res Result) error {
	var errs []error
	for _, tr := range res.Tasks {
		if _, err := removeTask(res, tr); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", tr.ID, err))
		}
	}
	return errors.Join(errs...)
}

// RemoveTasks returns removed task IDs even when other named tasks fail.
func RemoveTasks(res Result, ids []string) ([]string, error) {
	tasks := make([]TaskResult, 0, len(ids))
	for _, id := range ids {
		tr, err := res.Find(id)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, tr)
	}
	var errs []error
	var removed []string
	for _, tr := range tasks {
		didRemove, err := removeTask(res, tr)
		if didRemove {
			removed = append(removed, tr.ID)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", tr.ID, err))
		}
	}
	return removed, errors.Join(errs...)
}

var branchProbe = func(ctx context.Context, dir, ref string) (string, error) {
	return gitOut(ctx, dir, "for-each-ref", "--format=%(refname)", ref)
}

func removeTask(res Result, tr TaskResult) (bool, error) {
	var errs []error
	removed := false
	for _, c := range tr.Candidates {
		if !c.BranchMade || c.Branch == "" || c.Branch == tr.Branch {
			continue
		}
		did, err := removeTask(res, TaskResult{ID: c.Branch, Status: StatusRunning, Branch: c.Branch, Worktree: c.Worktree, Slot: c.Slot, BranchMade: c.BranchMade})
		removed = removed || did
		if err != nil {
			errs = append(errs, err)
		}
	}
	did, err := removeTaskClaim(res, tr)
	if err != nil {
		errs = append(errs, err)
	}
	return removed || did, errors.Join(errs...)
}

func removeTaskClaim(res Result, tr TaskResult) (bool, error) {
	// Bound probes; claim removal only after both probes succeed.
	branchState := func() (exists, ok bool) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		ref := "refs/heads/" + tr.Branch
		out, err := branchProbe(ctx, res.Dir, ref)
		if err != nil {
			return false, false
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.TrimSpace(line) == ref {
				return true, true
			}
		}
		return false, true
	}
	hadBranch := func() bool { exists, ok := branchState(); return ok && exists }
	branchGone := func() bool { exists, ok := branchState(); return ok && !exists }
	if branchOnly(tr) {
		had := hadBranch()
		err := removeSlotBranch(res.Dir, tr)
		return had && branchGone(), err
	}
	if tr.Worktree == "" || notMine(res, tr) {
		return false, nil
	}
	// Deregistered directories' status describes the surrounding repo.
	registered, err := isWorktree(res.Dir, tr.Worktree)
	if err != nil {
		return false, err
	}
	if registered {
		if dirty, err := hasChanges(tr.Worktree); err == nil && dirty {
			return false, fmt.Errorf("%s has uncommitted changes; commit or discard them first", tr.Worktree)
		}
	}
	had := hadBranch()
	err = removeWorktree(res.Dir, tr)
	stillRegistered, checkErr := isWorktree(res.Dir, tr.Worktree)
	return (registered && checkErr == nil && !stillRegistered) || (had && branchGone()), err
}

// Only BranchMade proves slotless ownership; legacy records cannot.
func branchOnly(tr TaskResult) bool {
	return tr.Slot != 0 || (tr.Worktree == "" && tr.Status == StatusRunning && tr.BranchMade)
}

// Paths precede creation; only BranchMade proves ownership.
// Legacy records instead exclude tasks that failed because the path or branch was taken.
func notMine(res Result, tr TaskResult) bool {
	if res.Ownership >= OwnershipClaim {
		return !tr.BranchMade
	}
	return foundTaken(tr)
}

// Match legacy pre-attempt failures only; hooks can say "already exists".
func foundTaken(tr TaskResult) bool {
	return tr.Slot == 0 && !tr.BranchMade && len(tr.Attempts) == 0 &&
		(tr.Error == takenError("branch", tr.Branch) || tr.Error == takenError("worktree", tr.Worktree))
}

// Keep text stable for legacy ownership checks.
func takenError(what, name string) string {
	return fmt.Sprintf("%s %s already exists; remove it or pick another task id", what, name)
}

// Never delete a branch currently checked out by a task or operator.
func removeSlotBranch(root string, tr TaskResult) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := gitOut(ctx, root, "worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	for _, block := range strings.Split(out, "\n\n") {
		var path, branch string
		for _, line := range strings.Split(block, "\n") {
			if p, ok := strings.CutPrefix(line, "worktree "); ok {
				path = p
			}
			if b, ok := strings.CutPrefix(line, "branch refs/heads/"); ok {
				branch = b
			}
		}
		if branch == tr.Branch {
			return fmt.Errorf("branch %s is checked out in %s (a task still running?), so it stays", tr.Branch, filepath.FromSlash(path))
		}
	}
	if _, err := gitOut(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+tr.Branch); err != nil {
		return nil // gone already
	}
	_, err = gitOut(ctx, root, "branch", "-D", tr.Branch)
	return err
}

func isWorktree(root, path string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if failWorktreeList != nil {
		if err := failWorktreeList(); err != nil {
			return false, err
		}
	}
	out, err := gitOut(ctx, root, "worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	want := comparablePath(path)
	for _, line := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok && comparablePath(p) == want {
			return true, nil
		}
	}
	return false, nil
}

// Resolve Windows short names and separators before ignoring case.
func comparablePath(p string) string {
	p = filepath.Clean(filepath.FromSlash(p))
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

func addWorktree(ctx context.Context, root string, tr TaskResult, base string) error {
	worktreeMu.Lock()
	defer worktreeMu.Unlock()
	g, err := newRepoGit(ctx, root)
	if err != nil {
		return err
	}
	defer g.close()
	oid, err := gitOutEnv(ctx, root, g.env(ctx), "", "rev-parse", "--verify", base+"^{commit}")
	if err != nil {
		return err
	}
	branch := exec.CommandContext(ctx, "git", "-C", root, "branch", tr.Branch, oid)
	nowindow.Set(branch)
	branch.Env = append(gitenv.WithoutRepoVars(branch.Environ()), g.env(ctx)...)
	if out, err := branch.CombinedOutput(); err != nil {
		return fmt.Errorf("git branch: %s: %w", strings.TrimSpace(string(out)), err)
	}
	_, pathErr := os.Lstat(tr.Worktree)
	pathAbsent := errors.Is(pathErr, os.ErrNotExist)
	// Never retry: a failed add may have registered the worktree.
	cmd := exec.CommandContext(ctx, "git", "-C", root, "worktree", "add", "-q", tr.Worktree, tr.Branch)
	nowindow.Set(cmd)
	cmd.Env = append(gitenv.WithoutRepoVars(cmd.Environ()), g.env(ctx)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		addErr := fmt.Errorf("git worktree add: %s: %w", strings.TrimSpace(string(out)), err)
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		registered, listErr := isWorktree(root, tr.Worktree)
		if pathAbsent {
			if listErr == nil && registered {
				if _, removeErr := gitOut(cleanupCtx, root, "worktree", "remove", "--force", tr.Worktree); removeErr != nil {
					return errors.Join(addErr, removeErr)
				}
			}
		}
		if pathAbsent || (listErr == nil && !registered) {
			if _, deleteErr := gitOut(cleanupCtx, root, "update-ref", "-d", "refs/heads/"+tr.Branch, oid); deleteErr != nil {
				return errors.Join(addErr, deleteErr)
			}
		}
		return addErr
	}
	return nil
}

// Only registered worktrees or surviving branches prove ownership.
// If listing fails, trust only the branch; stray paths may be reused.
func stillMade(root string, tr TaskResult) (made bool, left string) {
	registered, lerr := isWorktree(root, tr.Worktree)
	registered = lerr == nil && registered
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, berr := gitOut(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+tr.Branch)
	var parts []string
	switch _, serr := os.Stat(tr.Worktree); {
	case registered:
		parts = append(parts, "worktree "+tr.Worktree)
	case lerr != nil && serr == nil:
		parts = append(parts, "worktree or directory "+tr.Worktree)
	case serr == nil:
		parts = append(parts, "directory "+tr.Worktree)
	}
	if berr == nil {
		parts = append(parts, "branch "+tr.Branch)
	}
	switch len(parts) {
	case 0:
		left = "nothing is left"
	case 1:
		left = parts[0] + " is left"
	default:
		left = strings.Join(parts, " and ") + " are left"
	}
	return registered || berr == nil, left
}

func removeWorktree(root string, tr TaskResult) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	worktreeMu.Lock()
	defer worktreeMu.Unlock()
	var leftover error
	registered, err := isWorktree(root, tr.Worktree)
	switch {
	case err != nil:
		return err
	case registered:
		if _, err := gitOut(ctx, root, "worktree", "remove", "--force", tr.Worktree); err != nil {
			return err
		}
	default:
		// Windows locks leave deregistered directories; remove only if empty.
		_, _ = gitOut(ctx, root, "worktree", "prune")
		leftover = RemoveEmptyDir(tr.Worktree)
	}
	if _, err := gitOut(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+tr.Branch); err == nil {
		if _, err := gitOut(ctx, root, "branch", "-D", tr.Branch); err != nil {
			return errors.Join(leftover, err)
		}
	}
	return leftover
}

// RemoveEmptyDir removes dir only when empty: files in a directory git no longer lists are in no branch.
func RemoveEmptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case len(entries) > 0:
		what := fmt.Sprintf("%d entries", len(entries))
		if len(entries) == 1 {
			what = entries[0].Name()
		}
		return fmt.Errorf("%s is not a git worktree any more but still holds %s; look at it, then delete the directory", dir, what)
	}
	return os.Remove(dir)
}
