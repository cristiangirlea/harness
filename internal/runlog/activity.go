package runlog

import (
	"os"
	"path/filepath"
	"time"
)

// Watch and prune share the writer-liveness window.
const activeWindow = time.Hour

// Active reports a recent, nonzero write time.
func Active(last time.Time) bool {
	return !last.IsZero() && time.Since(last) < activeWindow
}

// LastWrite returns the latest transcript/result mtime, falling back to the directory or zero on failure.
// Results count because batch gates can outlive a run.
func LastWrite(id string) time.Time {
	if validID(id) != nil {
		return time.Time{}
	}
	dir := filepath.Join(Root(), id)
	info, err := os.Stat(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		info, err = os.Stat(dir)
	}
	var last time.Time
	if err == nil {
		last = info.ModTime()
	}
	if result, err := os.Stat(filepath.Join(dir, "result.json")); err == nil && result.ModTime().After(last) {
		last = result.ModTime()
	}
	return last
}

func stillRunning(id string) bool {
	if _, err := os.Stat(filepath.Join(Root(), id, "result.json")); err == nil {
		return false
	}
	return Active(LastWrite(id))
}
