package runlog

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
)

func TestWriterCountsFailedEventsAndSavesRecordError(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := Create("stub", agent.Task{Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.events.Close(); err != nil {
		t.Fatal(err)
	}
	w.Event(agent.Event{Kind: agent.EventText, Text: "first"})
	w.Event(agent.Event{Kind: agent.EventText, Text: "second"})
	recordErr := w.Err()
	if recordErr == nil || !strings.Contains(recordErr.Error(), "2 events were not recorded") || !errors.Is(recordErr, os.ErrClosed) {
		t.Fatalf("Err = %v, want two lost events wrapping os.ErrClosed", recordErr)
	}
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err == nil || !strings.Contains(err.Error(), recordErr.Error()) || !errors.Is(err, os.ErrClosed) {
		t.Errorf("Close = %v, want the event write error", err)
	}
	if _, err := os.Stat(filepath.Join(w.Dir(), "result.json")); err != nil {
		t.Fatal(err)
	}
	rec, err := Load(w.ID())
	if err != nil || !rec.Done || rec.RecordError != recordErr.Error() || rec.Result.Status != agent.StatusOK {
		t.Fatalf("saved record = %+v, err = %v", rec, err)
	}
}

func TestWriterWithSuccessfulEventsHasNoRecordError(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := Create("stub", agent.Task{Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Err(); err != nil {
		t.Fatal(err)
	}
	w.Event(agent.Event{Kind: agent.EventText, Text: "saved"})
	if err := w.Err(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	rec, err := Load(w.ID())
	if err != nil || !rec.Done || rec.RecordError != "" {
		t.Fatalf("saved record = %+v, err = %v", rec, err)
	}
	events, err := Events(w.ID())
	if err != nil || len(events) != 1 || events[0].Text != "saved" {
		t.Fatalf("events = %+v, err = %v", events, err)
	}
	data, err := os.ReadFile(filepath.Join(w.Dir(), "result.json"))
	if err != nil || strings.Contains(string(data), "record_error") {
		t.Fatalf("record_error must be omitted on success: %s, err = %v", data, err)
	}
}

func TestWriterDoesNotCountPartialEventsAsLost(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := Create("stub", agent.Task{Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.events.Close(); err != nil {
		t.Fatal(err)
	}
	w.Event(agent.Event{Kind: agent.EventText, Text: "fragment", Partial: true})
	if err := w.Err(); err != nil {
		t.Fatalf("partial event counted as lost: %v", err)
	}
	w.Event(agent.Event{Kind: agent.EventText, Text: "complete"})
	w.Event(agent.Event{Kind: agent.EventText, Text: "fragment", Partial: true})
	if err := w.Err(); err == nil || !strings.Contains(err.Error(), "1 events were not recorded") {
		t.Fatalf("Err = %v, want only the complete event counted", err)
	}
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err == nil {
		t.Fatal("Close must report the lost complete event")
	}
}

func TestWriterRetriesWritesAndKeepsTheFirstError(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := Create("stub", agent.Task{Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.events.Close(); err != nil {
		t.Fatal(err)
	}
	w.Event(agent.Event{Kind: agent.EventText, Text: "lost"})
	// Replace the closed file to simulate storage becoming writable again.
	w.events, err = os.OpenFile(filepath.Join(w.Dir(), "events.jsonl"), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.events.Close() })
	w.Event(agent.Event{Kind: agent.EventText, Text: "saved"})
	if err := w.Err(); err == nil || !strings.Contains(err.Error(), "1 events were not recorded") {
		t.Fatalf("successful retry must not count as lost: %v", err)
	}
	// A later, different encoding error must not replace the first write error.
	w.Event(agent.Event{Kind: agent.EventToolCall, Input: json.RawMessage(`{`)})
	if err := w.Err(); err == nil || !strings.Contains(err.Error(), "2 events were not recorded") || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Err = %v, want two failures wrapping the first error", err)
	}
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err == nil {
		t.Fatal("Close must report the earlier failures")
	}
	events, err := Events(w.ID())
	if err != nil || len(events) != 1 || events[0].Text != "saved" {
		t.Fatalf("writer must try again after a write failure: events = %+v, err = %v", events, err)
	}
}

func TestWriterCloseJoinsEventFileAndResultErrors(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := Create("stub", agent.Task{Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.events.Close(); err != nil {
		t.Fatal(err)
	}
	w.Event(agent.Event{Kind: agent.EventText, Text: "lost"})
	resultPath := filepath.Join(w.Dir(), "result.json")
	if err := os.Mkdir(resultPath, 0o755); err != nil {
		t.Fatal(err)
	}
	err = w.Close(agent.Result{Status: agent.StatusOK})
	if err == nil || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Close = %v, want joined errors wrapping os.ErrClosed", err)
	}
	for _, want := range []string{"1 events were not recorded", "close ", resultPath} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Close = %v, missing %q", err, want)
		}
	}
}

func TestWriterErrCanBeReadWhileEventsFailConcurrently(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := Create("stub", agent.Task{Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.events.Close(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 25 {
				w.Event(agent.Event{Kind: agent.EventText, Text: "lost"})
				if err := w.Err(); err == nil || !errors.Is(err, os.ErrClosed) {
					t.Errorf("Err = %v, want the first write error", err)
				}
			}
		})
	}
	wg.Wait()
	if err := w.Err(); err == nil || !strings.Contains(err.Error(), "100 events were not recorded") {
		t.Fatalf("Err = %v, want all concurrent failures counted", err)
	}
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err == nil {
		t.Fatal("Close must report the failures")
	}
}

func TestLoadAcceptsRecordsWithoutRecordError(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	const id = "20260101-000000-aaaaaa"
	write(t, filepath.Join(Root(), id), "result.json",
		`{"id":"`+id+`","backend":"stub","started":"2026-01-01T00:00:00Z","task":{"prompt":"old","dir":"."},"result":{"backend":"stub","status":"ok","final":"saved","turns":1,"duration_ns":42}}`)
	rec, err := Load(id)
	if err != nil || !rec.Done || rec.RecordError != "" || rec.ID != id || rec.Backend != "stub" || rec.Task.Prompt != "old" || rec.Result.Final != "saved" || rec.Result.Duration != 42 {
		t.Fatalf("old record = %+v, err = %v", rec, err)
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListSinceSkipsOldRunsAndCountsUnreadable(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	task := agent.Task{Prompt: "p", Dir: t.TempDir(), Options: agent.Options{Mode: agent.ModeAsk}}
	for _, backend := range []string{"api", "claude"} {
		w, err := Create(backend, task)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Close(agent.Result{Backend: backend, Status: agent.StatusOK}); err != nil {
			t.Fatal(err)
		}
	}
	// An old run, written by hand with the id its clock would have given it.
	const old = "20200101-000000-old000"
	write(t, filepath.Join(Root(), old), "task.json",
		`{"id":"`+old+`","backend":"api","started":"2020-01-01T00:00:00Z","task":{"prompt":"p","dir":"/x","mode":"ask"}}`)
	// A record that cannot be read, recent enough to be opened.
	write(t, filepath.Join(Root(), "20990101-000000-broken"), "result.json", "{")

	recs, unreadable, err := ListSince(time.Time{})
	if err != nil || len(recs) != 3 || unreadable != 1 {
		t.Fatalf("all: %d records, %d unreadable, err=%v", len(recs), unreadable, err)
	}
	recs, unreadable, err = ListSince(time.Now().Add(-time.Hour))
	if err != nil || len(recs) != 2 || unreadable != 1 {
		t.Fatalf("window: %d records, %d unreadable, err=%v", len(recs), unreadable, err)
	}
	for _, r := range recs {
		if r.ID == old {
			t.Error("a run older than the window must be skipped")
		}
	}
	if recs, err := List(1); err != nil || len(recs) != 1 {
		t.Errorf("List(1) = %d records, err=%v", len(recs), err)
	}
}

func TestIDBefore(t *testing.T) {
	cut := time.Date(2026, 9, 21, 12, 0, 0, 0, time.Local)
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"20260921-110000-abc123", true},
		{"20260921-130000-abc123", false},
		{"20260920-235959", true},
		{"not-a-run", false},
		{"", false},
		{"2026", false},
	} {
		if got := idBefore(tc.id, cut); got != tc.want {
			t.Errorf("idBefore(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
	if idBefore("20200101-000000-x", time.Time{}) {
		t.Error("the zero cut-off must never match")
	}
}

func TestContinuingRecordsWhatItFollows(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	first, err := Create("api", agent.Task{Prompt: "first", Dir: "."})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	if rec, err := Load(first.ID()); err != nil || rec.Continues != "" {
		t.Fatalf("a plain run follows nothing: %+v err=%v", rec, err)
	}

	second, err := CreateContinuing("api", agent.Task{Prompt: "and now?", Dir: "."}, first.ID())
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	rec, err := Load(second.ID())
	if err != nil || rec.Continues != first.ID() {
		t.Fatalf("continues = %q, want %q (err=%v)", rec.Continues, first.ID(), err)
	}
}

func TestLoadConversation(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := Create("api", agent.Task{Prompt: "first", Dir: "."})
	if err != nil {
		t.Fatal(err)
	}
	// Close releases the transcript file; Windows will not remove an open one.
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadConversation(w.ID()); err == nil || !strings.Contains(err.Error(), "no saved conversation") {
		t.Fatalf("err = %v, want a clear message about the missing conversation", err)
	}

	data, _ := json.Marshal(agent.Conversation{Model: "claude-opus-5"})
	if err := os.WriteFile(filepath.Join(w.Dir(), "messages.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	rec, conv, err := LoadConversation(w.ID())
	if err != nil || conv.Model != "claude-opus-5" || rec.Task.Prompt != "first" {
		t.Fatalf("rec=%+v conv=%+v err=%v", rec.Task, conv, err)
	}

	for _, bad := range []string{"../escape", "a/b", "", "sub" + string(filepath.Separator) + "x"} {
		if _, _, err := LoadConversation(bad); err == nil || !strings.Contains(err.Error(), "invalid run id") {
			t.Errorf("LoadConversation(%q) = %v, want it refused by id validation", bad, err)
		}
	}
}

func TestChainFollowsContinuesOldestFirst(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	task := agent.Task{Prompt: "p", Dir: t.TempDir()}
	var ids []string
	prev := ""
	for _, backend := range []string{"claude", "codex", "claude"} {
		w, err := CreateContinuing(backend, task, prev)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Close(agent.Result{Backend: backend, Status: agent.StatusOK}); err != nil {
			t.Fatal(err)
		}
		prev = w.ID()
		ids = append(ids, prev)
	}
	chain, missing, err := Chain(ids[2])
	if err != nil || missing != "" || len(chain) != 3 {
		t.Fatalf("chain = %+v, missing = %q, err = %v", chain, missing, err)
	}
	for i, rec := range chain {
		if rec.ID != ids[i] {
			t.Errorf("chain[%d] = %s, want %s", i, rec.ID, ids[i])
		}
	}
	if chain, _, err := Chain(ids[0]); err != nil || len(chain) != 1 {
		t.Errorf("a run that continues nothing is a chain of one: %+v err=%v", chain, err)
	}
}

func TestChainStopsAtAMissingRunAndRefusesALoop(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := CreateContinuing("codex", agent.Task{Prompt: "p"}, "20200101-000000-gone")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	chain, missing, err := Chain(w.ID())
	if err != nil || len(chain) != 1 || missing != "20200101-000000-gone" {
		t.Errorf("chain = %+v, missing = %q, err = %v", chain, missing, err)
	}

	// Records are written by hand here: hrn itself never makes a loop.
	a, b := "20260101-000000-aaaaaa", "20260101-000001-bbbbbb"
	write(t, filepath.Join(Root(), a), "result.json", `{"id":"`+a+`","continues":"`+b+`"}`)
	write(t, filepath.Join(Root(), b), "result.json", `{"id":"`+b+`","continues":"`+a+`"}`)
	if _, _, err := Chain(b); err == nil || !strings.Contains(err.Error(), "loops") {
		t.Errorf("a loop should be refused, got %v", err)
	}
	if _, _, err := Chain("20200101-000000-nope"); err == nil || !strings.Contains(err.Error(), "20200101-000000-nope") {
		t.Errorf("an unknown run should be named, got %v", err)
	}
}

func TestChainRefusesAnUnreadableAncestor(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	first, err := Create("codex", agent.Task{Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	second, err := CreateContinuing("claude", agent.Task{Prompt: "q"}, first.ID())
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	// The first run is there, but its record is half-written.
	if err := os.WriteFile(filepath.Join(first.Dir(), "result.json"), []byte("{half"), 0o644); err != nil {
		t.Fatal(err)
	}
	chain, missing, err := Chain(second.ID())
	if err == nil || !strings.Contains(err.Error(), "cannot be read") {
		t.Errorf("chain = %d runs, missing = %q, err = %v: an unreadable run is not a gone one", len(chain), missing, err)
	}
}
