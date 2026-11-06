package batch

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/tools"
	"github.com/cristiangirlea/harness/internal/ws"
)

// ErrMergeBusy refuses overlapping mergers of the same repository.
var ErrMergeBusy = errors.New("repository merge is busy")

// ErrMergedUnconfirmed preserves a merge whose exact commit cannot be proven.
var ErrMergedUnconfirmed = errors.New("merged; the commit could not be confirmed")

// MergeResult records one task's merge outcome.
type MergeResult struct {
	Task     string `json:"task"`
	Before   string `json:"before,omitempty"`
	Branch   string `json:"branch"`
	Commit   string `json:"commit,omitempty"`   // the merge or squash commit this run made
	Skipped  string `json:"skipped,omitempty"`  // why nothing was merged
	Conflict bool   `json:"conflict,omitempty"` // git reported conflicting paths
	Error    string `json:"error,omitempty"`    // any other failure, in git's words
}

// Merge merges passing tasks in spec order, restoring the tree on failure.
// It returns completed results with the error.
func Merge(ctx context.Context, res Result, only []string, squash bool) ([]MergeResult, error) {
	return merge(ctx, res, only, squash, "")
}

func merge(ctx context.Context, res Result, only []string, squash bool, message string) ([]MergeResult, error) {
	if res.Dir == "" {
		return nil, fmt.Errorf("batch %s has no repository recorded", res.ID)
	}
	tasks, err := mergeTasks(res, only)
	if err != nil {
		return nil, err
	}
	common, err := gitOut(ctx, res.Dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	common = filepath.FromSlash(common)
	if !filepath.IsAbs(common) {
		common = filepath.Join(res.Dir, common)
	}
	release, busy, err := ws.LockFile(filepath.Join(common, "hrn-merge.lock"), "hrn batch merge "+res.ID)
	if err != nil {
		return nil, err
	}
	if busy != "" {
		return nil, fmt.Errorf("%w: %s", ErrMergeBusy, busy)
	}
	defer release()
	lines, err := dirtyPaths(ctx, res.Dir, false)
	if err != nil {
		return nil, err
	}
	if len(lines) > 0 {
		return nil, fmt.Errorf("%s has uncommitted changes; merging would mix them with the agents' work. "+
			"Commit or stash them first:\n  %s", res.Dir, strings.Join(lines, "\n  "))
	}

	before, err := gitOut(ctx, res.Dir, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	var out []MergeResult
	for _, tr := range tasks {
		mr := MergeResult{Task: tr.ID, Branch: tr.Branch, Before: before}
		switch {
		case tr.Status != StatusPass:
			mr.Skipped = "did not pass (" + string(tr.Status) + ")"
		case tr.Commit == "":
			mr.Skipped = "changed nothing"
		case alreadyIn(ctx, res.Dir, tr.Branch):
			mr.Skipped = "already merged"
		}
		if mr.Skipped != "" {
			out = append(out, mr)
			continue
		}
		commit, conflicted, err := mergeBranch(ctx, res.Dir, tr, squash, message)
		switch {
		case errors.Is(err, errNothingStaged):
			mr.Skipped = "already merged"
		case errors.Is(err, ErrMergedUnconfirmed):
			mr.Error = ErrMergedUnconfirmed.Error()
			out = append(out, mr)
			return out, fmt.Errorf("%s: %w", tr.ID, err)
		case err != nil:
			mr.Conflict, mr.Error = conflicted, err.Error()
			out = append(out, mr)
			return out, fmt.Errorf("%s: %w", tr.ID, err)
		default:
			mr.Commit = commit
		}
		out = append(out, mr)
	}
	return out, nil
}

func alreadyIn(ctx context.Context, root, branch string) bool {
	_, err := gitOut(ctx, root, "merge-base", "--is-ancestor", branch, "HEAD")
	return err == nil
}

func mergeTasks(res Result, only []string) ([]TaskResult, error) {
	if len(only) == 0 {
		var out []TaskResult
		for _, tr := range res.Tasks {
			if tr.Status == StatusPass {
				out = append(out, tr)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("batch %s has no task that passed", res.ID)
		}
		return out, nil
	}
	var out []TaskResult
	for _, id := range only {
		tr, err := res.Find(id)
		if err != nil {
			return nil, err
		}
		out = append(out, tr)
	}
	return out, nil
}

var mergeReceiptRead = gitOut

var mergeStep = func(ctx context.Context, g *agentGit, args ...string) (string, error) {
	return g.out(ctx, args...)
}

var errNothingStaged = errors.New("nothing to commit")

// Hook, identity and cancellation failures are not conflicts.
func mergeBranch(ctx context.Context, root string, tr TaskResult, squash bool, message string) (commit string, conflicted bool, err error) {
	pre, err := gitOut(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return "", false, err
	}
	tip, err := gitOut(ctx, root, "rev-parse", tr.Branch+"^{commit}")
	if err != nil {
		return "", false, err
	}
	args := []string{"merge", "--no-ff", "--no-edit", tr.Branch}
	if squash {
		args = []string{"merge", "--squash", tr.Branch}
	} else if message != "" {
		args = []string{"merge", "--no-ff", "-m", message, tr.Branch}
	}
	g, err := newRepoGit(ctx, root)
	if err != nil {
		return "", false, err
	}
	defer g.close()
	if _, err := mergeStep(ctx, g, args...); err != nil {
		return failedMerge(g, root, pre, tip, squash, err)
	}
	if squash {
		if staged, err := hasStagedChanges(ctx, g); err != nil {
			return "", false, undoMerge(root, err)
		} else if !staged {
			return "", false, undoMerge(root, errNothingStaged)
		}
		if _, err := mergeStep(ctx, g, "commit", "-q", "-m", tr.ID+": "+tools.OneLine(tr.Prompt, 60)); err != nil {
			return failedMerge(g, root, pre, tip, squash, err)
		}
	}
	commit, _, err = readMergeReceipt(root, pre, tip, squash)
	return commit, false, err
}

// A command can fail after committing, so cleanup needs proof that HEAD stayed put.
func failedMerge(g *agentGit, root, pre, tip string, squash bool, cause error) (string, bool, error) {
	commit, unchanged, err := readMergeReceipt(root, pre, tip, squash)
	if !unchanged {
		return commit, false, err
	}
	return "", hasUnmerged(g), undoMerge(root, cause)
}

func readMergeReceipt(root, pre, tip string, squash bool) (commit string, unchanged bool, err error) {
	receiptCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	commit, err = mergeReceiptRead(receiptCtx, root, "rev-parse", "HEAD")
	if err != nil {
		return "", false, ErrMergedUnconfirmed
	}
	if commit == pre {
		return "", true, errors.New("merge did not advance HEAD")
	}
	parent, parentErr := gitOut(receiptCtx, root, "rev-parse", commit+"^1")
	verified := parentErr == nil && parent == pre
	if !squash {
		second, e := gitOut(receiptCtx, root, "rev-parse", commit+"^2")
		verified = verified && e == nil && second == tip
	}
	if !verified {
		return "", false, ErrMergedUnconfirmed
	}
	return commit, false, nil
}

// Use a fresh context during canceled-merge cleanup.
func hasUnmerged(g *agentGit) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := g.out(ctx, "ls-files", "--unmerged")
	return err == nil && out != ""
}

func hasStagedChanges(ctx context.Context, g *agentGit) (bool, error) {
	// Ask for names: --quiet conflates differences and command errors.
	out, err := g.out(ctx, "diff", "--cached", "--name-only")
	return out != "", err
}

// Cleanup outlives cancellation; squash has no MERGE_HEAD to abort.
// Reset to HEAD is safe only after the clean-tree check.
func undoMerge(root string, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	g, err := newRepoGit(ctx, root)
	if err != nil {
		return fmt.Errorf("%w; and the tree could not be restored: %v", cause, err)
	}
	defer g.close()
	if _, err := g.out(ctx, "merge", "--abort"); err == nil {
		return cause
	}
	if _, err := g.out(ctx, "reset", "--hard", "HEAD"); err != nil {
		return fmt.Errorf("%w; and the tree could not be restored: %v", cause, err)
	}
	return cause
}

// MergeAt merges one task's confirmed commit after checking that its branch still points there.
func MergeAt(ctx context.Context, res Result, task, commit string) ([]MergeResult, error) {
	tr, err := res.Find(task)
	if err != nil {
		return nil, err
	}
	head, err := BranchHead(ctx, res, task)
	if err != nil {
		return nil, err
	}
	if head != commit {
		return nil, fmt.Errorf("the branch moved")
	}
	copyRes := res
	copyRes.Tasks = append([]TaskResult(nil), res.Tasks...)
	for i := range copyRes.Tasks {
		if copyRes.Tasks[i].ID == task {
			copyRes.Tasks[i].Branch = head
		}
	}
	out, err := merge(ctx, copyRes, []string{task}, false, "Merge branch '"+tr.Branch+"'")
	for i := range out {
		out[i].Branch = tr.Branch
	}
	return out, err
}

// BranchHead resolves a task branch to its current commit.
func BranchHead(ctx context.Context, res Result, task string) (string, error) {
	tr, err := res.Find(task)
	if err != nil {
		return "", err
	}
	if res.Dir == "" || tr.Branch == "" {
		return "", fmt.Errorf("task has no repository or branch")
	}
	return gitOut(ctx, res.Dir, "rev-parse", "--verify", "refs/heads/"+tr.Branch+"^{commit}")
}

// TargetBranch returns the currently checked-out branch in the batch repository.
func TargetBranch(ctx context.Context, res Result) (string, error) {
	if res.Dir == "" {
		return "", fmt.Errorf("batch has no repository")
	}
	return gitOut(ctx, res.Dir, "symbolic-ref", "--short", "HEAD")
}
