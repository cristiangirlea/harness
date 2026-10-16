package watch

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// batchCache keeps each batch record as cleaned, keyed by its file's time
// and size, so the index and Ada do not clean every batch on every look.
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

// listBatches is batch.List with every record cleaned of secrets. Files are
// looked at before they are read: a record saved in between is kept under
// its older time and so is cleaned again on the next look, never left stale.
func (s *server) listBatches() ([]batch.Result, int, error) {
	files := map[string]os.FileInfo{}
	entries, _ := os.ReadDir(filepath.Join(runlog.Home(), "batches"))
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if info, err := e.Info(); ok && err == nil && info.Mode().IsRegular() {
			files[id] = info
		}
	}
	batches, unreadable, err := batch.List()
	c := &s.batches
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := make(map[string]cachedBatch, len(batches))
	for i, res := range batches {
		info, onDisk := files[res.ID]
		if old, ok := c.m[res.ID]; ok && onDisk && old.mod.Equal(info.ModTime()) && old.size == info.Size() {
			batches[i] = old.res
			kept[res.ID] = old
			continue
		}
		batches[i] = s.cleanBatch(res)
		c.cleans++
		if onDisk {
			kept[res.ID] = cachedBatch{mod: info.ModTime(), size: info.Size(), res: batches[i]}
		}
	}
	c.m = kept
	return batches, unreadable, err
}
