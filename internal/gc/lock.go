package gc

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/ws"
)

type lockInfo struct {
	PID   int       `json:"pid"`
	Start string    `json:"start,omitempty"`
	Time  time.Time `json:"time"`
}

// LockHeldError reports when another cleanup owns the lock.
type LockHeldError struct{ Since time.Time }

func (e *LockHeldError) Error() string {
	return fmt.Sprintf("gc.lock held since %s", e.Since.Format(time.RFC3339))
}

func lockDetails(path string) (lockInfo, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return lockInfo{}, false, nil
	}
	if err != nil {
		return lockInfo{}, false, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return lockInfo{}, false, err
	}
	l := lockInfo{Time: info.ModTime()}
	if json.Unmarshal(b, &l) != nil || l.Time.IsZero() {
		l = lockInfo{Time: info.ModTime()}
	}
	return l, true, nil
}

// LockWarning reports a lock that has remained present for the configured interval.
func LockWarning(now time.Time) string {
	c, err := LoadConfig()
	if err != nil || !c.Enabled {
		return ""
	}
	l, exists, err := lockDetails(filepath.Join(runlog.Home(), "gc.lock"))
	if err != nil || !exists {
		return ""
	}
	d, _ := time.ParseDuration(c.Interval)
	if now.Sub(l.Time) < d {
		return ""
	}
	return (&LockHeldError{Since: l.Time}).Error()
}

// Lock serializes manual and automatic cleanup and returns its release function.
func Lock(now time.Time) (func(), error) {
	if err := os.MkdirAll(runlog.Home(), 0700); err != nil {
		return nil, err
	}
	guard, ok, err := ws.TryLock(filepath.Join(runlog.Home(), "gc.guard"))
	if err != nil {
		return nil, err
	}
	path := filepath.Join(runlog.Home(), "gc.lock")
	if !ok {
		l, _, _ := lockDetails(path)
		return nil, &LockHeldError{Since: l.Time}
	}
	l, exists, err := lockDetails(path)
	if err != nil {
		guard()
		return nil, err
	}
	if exists {
		stale := now.Sub(l.Time) >= 6*time.Hour || (l.PID > 0 && !ws.ProcessAlive(l.PID, l.Start))
		if !stale {
			guard()
			return nil, &LockHeldError{Since: l.Time}
		}
		if err := appendPass(Pass{Time: now, Error: fmt.Sprintf("took over gc.lock held since %s by pid %d", l.Time.Format(time.RFC3339), l.PID)}); err != nil {
			guard()
			return nil, err
		}
	}
	start, _ := ws.StartID(os.Getpid())
	b, _ := json.Marshal(lockInfo{PID: os.Getpid(), Start: start, Time: now})
	if err := writeAtomic(path, b); err != nil {
		guard()
		return nil, err
	}
	return func() { os.Remove(path); guard() }, nil
}
