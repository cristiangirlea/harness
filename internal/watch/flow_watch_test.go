package watch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFlowFileWatcherDetectsChanges(t *testing.T) {
	for _, change := range []string{"creation", "size", "mtime"} {
		t.Run(change, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			t.Setenv("HRN_OWNER", "local:test")
			path := filepath.Join(os.Getenv("HRN_HOME"), "events.jsonl")
			before := time.Now().Add(-time.Hour).Truncate(time.Second)
			if change != "creation" {
				if err := os.WriteFile(path, []byte("first"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, before, before); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			changed := watchFlowFile(ctx, path, 400*time.Millisecond)
			if change == "mtime" {
				if err := os.Chtimes(path, before.Add(time.Minute), before.Add(time.Minute)); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path, []byte("a longer event"), 0o644); err != nil {
					t.Fatal(err)
				}
				if change == "size" {
					if err := os.Chtimes(path, before, before); err != nil {
						t.Fatal(err)
					}
				}
			}
			select {
			case <-changed:
			case <-time.After(250 * time.Millisecond):
				t.Fatal("file change did not wake the watcher within 250ms")
			}
		})
	}
}
