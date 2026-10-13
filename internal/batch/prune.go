package batch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RemoveRecord deletes a saved batch record and returns how many bytes it
// held; with dryRun it only reports the size. It touches neither worktrees nor
// branches: hrn batch rm does that, and it needs the record to find them.
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

// WorktreesRemain reports whether any of the batch's worktrees is still on
// disk (not one a fresh task found taken, foundTaken), or for a slot task (whose slot outlives the batch) or one that waited
// for a slot (branchOnly) its branch, which is when its record must stay:
// hrn batch rm reads it.
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
		if t.Worktree == "" || foundTaken(t) {
			continue
		}
		if _, err := os.Stat(t.Worktree); err == nil {
			return true
		}
	}
	return false
}
