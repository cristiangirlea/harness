// Package limits stores remembered usage-limit windows.
package limits

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// Atomic writes prevent partial reads; lost updates cost a limited start.
const fileName = "limits.json"

var mu sync.RWMutex

type entry struct {
	Until  time.Time `json:"until"`
	Reason string    `json:"reason,omitempty"`
}

func path() string { return filepath.Join(runlog.Home(), fileName) }

// Retry Windows sharing conflicts.
func retry(f func() error) error {
	var err error
	for i := 0; i < 20; i++ {
		if err = f(); err == nil || errors.Is(err, fs.ErrNotExist) {
			return err
		}
		time.Sleep(25 * time.Millisecond)
	}
	return err
}

// Caller holds mu; unreadable state costs a limited start.
func load() map[string]entry {
	var data []byte
	if retry(func() (err error) { data, err = os.ReadFile(path()); return err }) != nil {
		return map[string]entry{}
	}
	var m map[string]entry
	if json.Unmarshal(data, &m) != nil || m == nil {
		return map[string]entry{}
	}
	return m
}

// Note persists a known limit reset.
func Note(backend string, res agent.Result) error {
	if res.Status != agent.StatusLimited || res.LimitResets.IsZero() {
		return nil
	}
	mu.Lock()
	defer mu.Unlock()
	m := load()
	now := time.Now()
	for name, e := range m {
		if !e.Until.After(now) {
			delete(m, name)
		}
	}
	m[backend] = entry{Until: res.LimitResets, Reason: res.Error}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path(), data)
}

func writeFile(name string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(name), fileName+".*")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = retry(func() error { return os.Rename(f.Name(), name) })
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// Until returns a reset or zero, bounding implausibly long diversions.
func Until(backend string, now time.Time) time.Time {
	mu.RLock()
	m := load()
	mu.RUnlock()
	if e, ok := m[backend]; ok && e.Until.After(now) && !e.Until.After(now.Add(agent.MaxLimitWindow)) {
		return e.Until
	}
	return time.Time{}
}

// Status describes a limit or returns "", naming the file for early-reset cleanup.
func Status(backend string, now time.Time) string {
	until := Until(backend, now)
	if until.IsZero() {
		return ""
	}
	return fmt.Sprintf("at its usage limit until %s, as remembered in %s (delete it if the limit has lifted)", when(until), path())
}

func when(t time.Time) string { return t.Local().Format("Jan 2 15:04") }
