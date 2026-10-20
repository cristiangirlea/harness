package watch

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// Cache cleaned records so polling does not repeatedly redact unchanged batches.
type batchCache struct {
	mu     sync.Mutex
	m      map[string]cachedBatch
	cleans int // how many records were cleaned, for tests
}

type cachedBatch struct {
	mod  time.Time
	size int64
	res  batch.Result
}

// listBatches is batch.List with every record cleaned; stat before loading so a concurrent save is cleaned again next poll.
func (s *server) listBatches() (batches []batch.Result, unreadable int, err error) {
	entries, err := os.ReadDir(filepath.Join(runlog.Home(), "batches"))
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	c := &s.batches
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := make(map[string]cachedBatch, len(entries))
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if e.IsDir() || !ok {
			continue
		}
		info, statErr := e.Info()
		regular := statErr == nil && info.Mode().IsRegular()
		if old, ok := c.m[name]; ok && regular && old.mod.Equal(info.ModTime()) && old.size == info.Size() {
			batches = append(batches, old.res)
			kept[name] = old
			continue
		}
		res, loadErr := batch.LoadResult(name)
		if loadErr != nil {
			unreadable++
			continue
		}
		res = s.cleanBatch(res)
		c.cleans++
		batches = append(batches, res)
		if regular {
			kept[name] = cachedBatch{mod: info.ModTime(), size: info.Size(), res: res}
		}
	}
	c.m = kept
	sort.Slice(batches, func(i, j int) bool { return batches[i].Started.After(batches[j].Started) })
	return batches, unreadable, err
}
