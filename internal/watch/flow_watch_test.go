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
			case <-time.After(5 * time.Second):
				t.Fatal("file change did not wake the watcher within 5s")
			}
		})
	}
}

func TestTheFlowFileWatcherWakesAtMostOncePerPoll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	poll := 400 * time.Millisecond
	changed := watchFlowFile(ctx, path, poll)
	stop := time.After(1200 * time.Millisecond)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	wakes := 0
	write := time.NewTicker(20 * time.Millisecond)
	defer write.Stop()
	for done := false; !done; {
		select {
		case <-write.C:
			if _, err := f.WriteString("event\n"); err != nil {
				t.Fatal(err)
			}
		case <-changed:
			wakes++
		case <-stop:
			done = true
		}
	}
	// A run writing constantly for three polls may wake the stream three or four times, never twelve.
	if wakes < 1 || wakes > 4 {
		t.Fatalf("%d wakes in three polls of constant writing", wakes)
	}
}

func TestAChangeDuringTheCooldownStillWakesOnceItPasses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	poll := 400 * time.Millisecond
	changed := watchFlowFile(ctx, path, poll)
	if err := os.WriteFile(path, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-time.After(poll):
		t.Fatal("no wake for the first write")
	}
	// The last event of a run lands inside the cooldown and nothing follows it.
	written := time.Now()
	if err := os.WriteFile(path, []byte("first\nlast\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
		if waited := time.Since(written); waited < poll/2 {
			t.Fatalf("woke after %s, inside the cooldown", waited)
		}
	case <-time.After(poll + poll):
		t.Fatal("a change made during the cooldown was dropped")
	}
}
