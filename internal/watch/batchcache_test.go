package watch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/mask"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestABatchIsCleanedAgainOnlyWhenItsRecordChanges(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	const secret = "Abcdefghijklmnop"
	s := &server{redact: mask.Env([]string{"SERVICE_TOKEN=" + secret})}
	start := time.Now()
	old := batch.Result{ID: "20261012-170000-abcdef", Started: start.Add(-time.Hour),
		Tasks: []batch.TaskResult{{ID: "t1", Prompt: secret + " first"}}}
	other := batch.Result{ID: "20261012-180000-abcdef", Started: start}
	saveBatch(t, old)
	saveBatch(t, other)
	list := func(want int) []batch.Result {
		t.Helper()
		got, unreadable, err := s.listBatches()
		if err != nil || unreadable != 0 || len(got) != want {
			t.Fatalf("listBatches = %d batches, %d unreadable, %v; want %d", len(got), unreadable, err, want)
		}
		for _, res := range got {
			for _, tr := range res.Tasks {
				if strings.Contains(tr.Prompt, secret) {
					t.Fatalf("batch %s shows the secret: %q", res.ID, tr.Prompt)
				}
			}
		}
		return got
	}
	cleans := func() int {
		s.batches.mu.Lock()
		defer s.batches.mu.Unlock()
		return s.batches.cleans
	}

	list(2)
	if n := cleans(); n != 2 {
		t.Fatalf("first look cleaned %d batches, want 2", n)
	}
	list(2)
	if n := cleans(); n != 2 {
		t.Fatalf("an unchanged batch was cleaned again: %d cleanings, want 2", n)
	}

	old.Tasks[0].Prompt = secret + " changed, and longer"
	saveBatch(t, old)
	got := list(2)
	if n := cleans(); n != 3 {
		t.Fatalf("after one record grew: %d cleanings, want 3", n)
	}
	if !strings.Contains(got[1].Tasks[0].Prompt, "changed, and longer") {
		t.Fatalf("the changed batch shows its old text: %q", got[1].Tasks[0].Prompt)
	}

	// The same size with a new time is a change too.
	old.Tasks[0].Prompt = secret + " CHANGED, and longer"
	saveBatch(t, old)
	path := filepath.Join(runlog.Home(), "batches", old.ID+".json")
	if err := os.Chtimes(path, start.Add(time.Minute), start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got = list(2)
	if n := cleans(); n != 4 || !strings.Contains(got[1].Tasks[0].Prompt, "CHANGED") {
		t.Fatalf("after a same-size rewrite: %d cleanings, want 4; prompt %q", n, got[1].Tasks[0].Prompt)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	list(1)
	s.batches.mu.Lock()
	_, kept := s.batches.m[old.ID]
	size := len(s.batches.m)
	s.batches.mu.Unlock()
	if kept || size != 1 {
		t.Fatalf("a removed batch stays in the cache (%v, %d entries)", kept, size)
	}
}

// A record names its own id, and a copied or hand-edited file may name
// another file's: each file still shows its own record.
func TestABatchFileHoldingAnotherFilesIDShowsItsOwnRecord(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	s := &server{}
	start := time.Now()
	first := batch.Result{ID: "20261012-170000-abcdef", Started: start.Add(-time.Hour),
		Tasks: []batch.TaskResult{{ID: "t1", Prompt: "first record"}}}
	saveBatch(t, first)
	copied := batch.Result{ID: first.ID, Started: start,
		Tasks: []batch.TaskResult{{ID: "t1", Prompt: "copied record"}}}
	data, _ := json.Marshal(copied)
	if err := os.WriteFile(filepath.Join(runlog.Home(), "batches", "20261012-180000-abcdef.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	for look := 1; look <= 2; look++ {
		got, unreadable, err := s.listBatches()
		if err != nil || unreadable != 0 || len(got) != 2 {
			t.Fatalf("look %d: %d batches, %d unreadable, %v; want 2", look, len(got), unreadable, err)
		}
		if got[0].Tasks[0].Prompt != "copied record" || got[1].Tasks[0].Prompt != "first record" {
			t.Fatalf("look %d shows %q and %q, want each file's own record", look, got[0].Tasks[0].Prompt, got[1].Tasks[0].Prompt)
		}
	}
	if s.batches.cleans != 2 {
		t.Fatalf("%d cleanings, want 2: an unchanged file was cleaned again", s.batches.cleans)
	}
}
