package runlog

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// PruneResult is what Prune removed, or would remove on a dry run.
type PruneResult struct {
	Removed []string `json:"removed"` // run ids, newest first
	Bytes   int64    `json:"bytes"`
}

// Prune removes the run directories whose id says they started before cutoff.
// These stay whatever their age: the newest keep runs; any run in protect,
// because a record that is being kept still cites it; a run with no result
// that wrote within the last hour, because it is probably still running; every
// run a kept run continues from, down the whole chain; and any directory whose
// name is not a run id, since nothing says how old it is. With dryRun it
// reports what it would remove and removes nothing.
func Prune(cutoff time.Time, keep int, protect map[string]bool, dryRun bool) (PruneResult, error) {
	var res PruneResult
	entries, err := os.ReadDir(Root())
	if errors.Is(err, os.ErrNotExist) {
		return res, nil
	}
	if err != nil {
		return res, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && isRunID(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	// Ids start with their timestamp, so the newest sort first.
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))

	remove := map[string]bool{}
	var kept []string
	for i, id := range ids {
		if i < keep || protect[id] || !idBefore(id, cutoff) || stillRunning(id) {
			kept = append(kept, id)
			continue
		}
		remove[id] = true
	}
	// A kept run keeps the run it continues, and that one keeps its own.
	for len(kept) > 0 {
		id := kept[0]
		kept = kept[1:]
		if rec, err := Load(id); err == nil && remove[rec.Continues] {
			delete(remove, rec.Continues)
			kept = append(kept, rec.Continues)
		}
	}

	for _, id := range ids {
		if !remove[id] {
			continue
		}
		dir := filepath.Join(Root(), id)
		size, err := dirSize(dir)
		if err != nil {
			return res, err
		}
		if !dryRun {
			if err := os.RemoveAll(dir); err != nil {
				return res, fmt.Errorf("removing run %s: %w", id, err)
			}
		}
		res.Removed = append(res.Removed, id)
		res.Bytes += size
	}
	return res, nil
}

// isRunID reports whether name is shaped like an id NewID makes, so its age
// can be read from it.
func isRunID(name string) bool {
	if validID(name) != nil || len(name) < len(idTimeLayout) {
		return false
	}
	_, err := time.ParseInLocation(idTimeLayout, name[:len(idTimeLayout)], time.Local)
	return err == nil
}

func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}
