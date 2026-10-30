package runlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// PruneResult lists actual or dry-run removals.
type PruneResult struct {
	Removed     []string `json:"removed"` // run ids, newest first
	Bytes       int64    `json:"bytes"`
	ScriptHints int      `json:"script_hints,omitempty"`
	OrphanFlows int      `json:"orphan_flows,omitempty"`
}

// Prune selects old runs except keep newest, protected ids, active writers and unknown names.
// Retain kept continuation chains; dryRun only reports.
func Prune(cutoff time.Time, keep int, protect map[string]bool, dryRun bool) (PruneResult, error) {
	return PruneOnly(cutoff, keep, protect, nil, dryRun)
}

// PruneOnly is Prune that removes no run outside only, so a caller can act on a dry run's list first.
func PruneOnly(cutoff time.Time, keep int, protect, only map[string]bool, dryRun bool) (PruneResult, error) {
	var res PruneResult
	entries, err := os.ReadDir(Root())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return res, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && isRunID(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))

	remove := map[string]bool{}
	var kept []string
	for i, id := range ids {
		if i < keep || protect[id] || only != nil && !only[id] || !idBefore(id, cutoff) || stillRunning(id) {
			kept = append(kept, id)
			continue
		}
		remove[id] = true
	}
	// Preserve every ancestor, not just the direct predecessor.
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
	err = pruneOrphanFlows(&res, dryRun)
	if err == nil {
		res.ScriptHints, err = pruneHints(cutoff, dryRun)
	}
	return res, err
}

func pruneHints(cutoff time.Time, dryRun bool) (int, error) {
	path := filepath.Join(Home(), "script-hints.jsonl")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var kept bytes.Buffer
	count := 0
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), len(data)+1)
	for scanner.Scan() {
		line := scanner.Bytes()
		var record struct {
			Time time.Time `json:"time"`
		}
		if json.Unmarshal(line, &record) != nil || record.Time.IsZero() || record.Time.Before(cutoff) {
			count++
			continue
		}
		kept.Write(line)
		kept.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	if dryRun || count == 0 {
		return count, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".script-hints-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(kept.Bytes())
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return 0, err
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return 0, err
	}
	return count, nil
}

func pruneOrphanFlows(res *PruneResult, dryRun bool) error {
	dir := filepath.Join(Home(), "batch-flows")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".jsonl")
		if !ok || !isRunID(id) || !e.Type().IsRegular() {
			continue
		}
		_, err := os.Lstat(filepath.Join(Home(), "batches", id+".json"))
		if err == nil {
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if !dryRun {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return fmt.Errorf("removing orphan flow %s: %w", id, err)
			}
		}
		res.OrphanFlows++
		res.Bytes += info.Size()
	}
	return nil
}

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
