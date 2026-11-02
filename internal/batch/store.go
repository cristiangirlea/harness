package batch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/runlog"
)

var (
	droppingBranch   func()
	rangeComputed    func(max int)
	failSave         func(Result) error
	failWorktreeList func() error
	saveRename       = (*os.Root).Rename
	saveSleep        = time.Sleep
)

func batchesDir() string { return filepath.Join(runlog.Home(), "batches") }

// SaveResult saves a batch record using the same path as Run.
func SaveResult(res Result) error { return save(res) }

func save(res Result) error {
	if failSave != nil {
		if err := failSave(res); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(batchesDir(), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	// Rename complete JSON so readers never see partial records.
	// Windows needs os.Root POSIX rename and FILE_SHARE_DELETE; retry other readers.
	root, err := os.OpenRoot(batchesDir())
	if err != nil {
		return err
	}
	defer root.Close()
	name := res.ID + ".json"
	if err := root.WriteFile(name+".tmp", data, 0o600); err != nil {
		return err
	}
	const budget = 2 * time.Second
	for delay, waited := 10*time.Millisecond, time.Duration(0); ; {
		err = saveRename(root, name+".tmp", name)
		if err == nil {
			return nil
		}
		if !renameBusy(err) || waited >= budget {
			_ = root.Remove(name + ".tmp")
			return err
		}
		pause := min(delay, budget-waited)
		saveSleep(pause)
		waited += pause
		delay = min(2*delay, 200*time.Millisecond)
	}
}

// List returns records newest first and counts unreadable files.
func List() (results []Result, unreadable int, err error) {
	entries, err := os.ReadDir(batchesDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if e.IsDir() || !ok {
			continue
		}
		res, err := LoadResult(id)
		if err != nil {
			unreadable++
			continue
		}
		results = append(results, res)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Started.After(results[j].Started) })
	return results, unreadable, nil
}

// Find returns the task with the given ID.
func (r Result) Find(id string) (TaskResult, error) {
	ids := make([]string, 0, len(r.Tasks))
	for _, tr := range r.Tasks {
		if tr.ID == id {
			return tr, nil
		}
		ids = append(ids, tr.ID)
	}
	return TaskResult{}, fmt.Errorf("batch %s has no task %q (it has %s)", r.ID, id, strings.Join(ids, ", "))
}

// LoadResult reads a saved batch record.
func LoadResult(id string) (Result, error) {
	var res Result
	if id == "" || strings.ContainsAny(id, `/\`) {
		return res, fmt.Errorf("invalid batch id %q", id)
	}
	data, err := readRecord(filepath.Join(batchesDir(), id+".json"))
	if err != nil {
		return res, err
	}
	return res, json.Unmarshal(data, &res)
}
