package runlog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunActivityUsesTheTranscriptOrDirectoryAndHonoursTheResult(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, transcript := range []bool{true, false} {
		for _, minutes := range []int{59, 61} {
			id := NewID()
			dir := filepath.Join(Root(), id)
			write(t, dir, "task.json", "{}")
			path := dir
			if transcript {
				write(t, dir, "events.jsonl", "")
				path = filepath.Join(dir, "events.jsonl")
			}
			at := time.Now().Add(-time.Duration(minutes) * time.Minute)
			if err := os.Chtimes(path, at, at); err != nil {
				t.Fatal(err)
			}
			if got := stillRunning(id); got != (minutes < 60) {
				t.Errorf("transcript=%v quiet=%dm: running=%v", transcript, minutes, got)
			}
			write(t, dir, "result.json", "{}")
			if stillRunning(id) {
				t.Error("a saved result must end the run")
			}
		}
	}
}
