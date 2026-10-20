package batch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RemoveRecord deletes a record and returns its size in bytes, or just measures it with dryRun.
// Remove worktrees first: batch rm needs the record to find them.
func RemoveRecord(id string, dryRun bool) (int64, error) {
	if id == "" || strings.ContainsAny(id, `/\`) {
		return 0, fmt.Errorf("invalid batch id %q", id)
	}
	path := filepath.Join(batchesDir(), id+".json")
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if !dryRun {
		if err := os.Remove(path); err != nil {
			return 0, err
		}
	}
	return info.Size(), nil
}

// WorktreesRemain reports owned worktrees or slot/waiting branches that still need the record for cleanup.
func WorktreesRemain(res Result) bool {
	for _, t := range res.Tasks {
		if branchOnly(t) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			_, err := gitOut(ctx, res.Dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+t.Branch)
			cancel()
			if err == nil {
				return true
			}
			continue
		}
		if t.Worktree == "" || notMine(res, t) {
			continue
		}
		if _, err := os.Stat(t.Worktree); err == nil {
			return true
		}
	}
	return false
}
