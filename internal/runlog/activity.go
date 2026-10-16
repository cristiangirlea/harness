package runlog

import (
	"os"
	"path/filepath"
	"time"
)

// activeWindow is how recently an unfinished record must have written to
// count as still running rather than stopped. Watch and prune share this rule.
const activeWindow = time.Hour

// Active reports whether last is recent enough to indicate a live writer.
// A zero time means no write was observed.
func Active(last time.Time) bool {
	return !last.IsZero() && time.Since(last) < activeWindow
}

// LastWrite is the transcript's modification time, falling back to the run
// directory if the transcript is absent, as prune has always done. A newer
// result also counts: a batch may still be running gates after its run ends.
// An invalid id or unreadable directory returns zero. Nothing is changed.
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

// stillRunning protects an unfinished run's active writer from prune.
func stillRunning(id string) bool {
	if _, err := os.Stat(filepath.Join(Root(), id, "result.json")); err == nil {
		return false
	}
	return Active(LastWrite(id))
}
