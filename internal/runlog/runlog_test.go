package runlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
)

func TestResolveHomeRelative(t *testing.T) {
	t.Setenv("HRN_HOME", "rel")
	want, err := filepath.Abs("rel")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ResolveHome()
	if err != nil || got != want {
		t.Fatalf("ResolveHome() = %q, %v; want %q", got, err, want)
	}
}

func TestMissingHomeRefusesRecords(t *testing.T) {
	for _, key := range []string{"HRN_HOME", "HOME", "USERPROFILE", "HOMEDRIVE", "HOMEPATH"} {
		t.Setenv(key, "")
	}
	if h := Home(); h != "" && !filepath.IsAbs(h) {
		t.Fatalf("relative home: %q", h)
	}
	if err := os.MkdirAll(filepath.Join(Home(), "other-records"), 0o755); err == nil {
		t.Fatal("legacy home consumer created a directory")
	}
	if _, err := Create("stub", agent.Task{}); err == nil {
		t.Fatal("created a record without a home")
	}
	if _, err := List(0); err == nil {
		t.Fatal("listed records without a home")
	}
	for name, read := range map[string]func() error{
		"Load":    func() error { _, err := Load("valid"); return err },
		"Events":  func() error { _, err := Events("valid"); return err },
		"TurnsOf": func() error { _, err := TurnsOf("valid"); return err },
	} {
		if err := read(); err == nil || !strings.Contains(err.Error(), "set HRN_HOME") {
			t.Errorf("%s error = %v, want set HRN_HOME", name, err)
		}
	}
}

func TestDotIDsAreInvalid(t *testing.T) {
	for _, id := range []string{".", ".."} {
		if err := validID(id); err == nil {
			t.Errorf("accepted %q", id)
		}
		if _, err := Load(id); err == nil {
			t.Errorf("loaded %q", id)
		}
	}
}

func TestLoadUsesDirectoryID(t *testing.T) {
	for _, file := range []string{"result.json", "task.json"} {
		for _, storedID := range []string{"../outside", "20261014-120000-abcdef"} {
			t.Run(file+"/"+storedID, func(t *testing.T) {
				t.Setenv("HRN_HOME", t.TempDir())
				id := "20261014-130000-abcdef"
				dir := filepath.Join(Root(), id)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				want := Record{ID: storedID, Backend: "stub", Task: agent.Task{Prompt: "keep me"}}
				if err := writeJSON(filepath.Join(dir, file), want); err != nil {
					t.Fatal(err)
				}
				rec, err := Load(id)
				if err != nil {
					t.Fatal(err)
				}
				if rec.ID != id || rec.Done != (file == "result.json") || rec.Backend != want.Backend || rec.Task.Prompt != want.Task.Prompt {
					t.Fatalf("loaded record = %+v, want ID %q and contents preserved", rec, id)
				}
				recs, err := List(0)
				if err != nil || len(recs) != 1 || recs[0].ID != id {
					t.Fatalf("listed records = %+v, error = %v", recs, err)
				}
			})
		}
	}
}

func TestOldTaskRecordsLoadWithoutAPurpose(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	id := "20261014-120000-abcdef"
	dir := filepath.Join(Root(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := `{"id":"` + id + `","backend":"stub","task":{"prompt":"Fix it.","dir":"."}}`
	if err := os.WriteFile(filepath.Join(dir, "task.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, err := Load(id)
	if err != nil || rec.Task.Purpose != "" || rec.Task.Prompt != "Fix it." {
		t.Fatalf("old record = %+v, error = %v", rec, err)
	}
	encoded, err := json.Marshal(rec.Task)
	if err != nil || strings.Contains(string(encoded), `"purpose"`) {
		t.Fatalf("empty purpose must be omitted: %s, error = %v", encoded, err)
	}
}

func TestARoutedRunKeepsItsRowAndAnOldRecordLoadsWithoutOne(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	by := &RoutedBy{Category: "implement", Declared: true, Eval: "e1", Set: "2026-10-12"}
	w, err := CreateContinuing("codex", agent.Task{Prompt: "Implement widget"}, "", by)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	r, err := Load(w.ID())
	if err != nil || r.RoutedBy == nil || *r.RoutedBy != *by {
		t.Fatalf("record: %+v %v", r, err)
	}
	data, err := os.ReadFile(filepath.Join(w.Dir(), "task.json"))
	if err != nil || !strings.Contains(string(data), "routed_by") {
		t.Fatalf("task: %s %v", data, err)
	}
	w, err = Create("codex", agent.Task{Prompt: "old"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	r, err = Load(w.ID())
	if err != nil || r.RoutedBy != nil {
		t.Fatalf("legacy: %+v %v", r, err)
	}
	data, _ = json.Marshal(r)
	if strings.Contains(string(data), "routed_by") {
		t.Fatalf("legacy gained field: %s", data)
	}
}

func TestRouteNoteIsSavedForTaskAndResultAndOldRecordsLoad(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := CreateRouted("stub", agent.Task{Prompt: "p"}, "", "", nil, "backend stub was named for this run")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(w.Dir(), "task.json"))
	if err != nil || !strings.Contains(string(data), `"route_note": "backend stub was named for this run"`) {
		t.Fatalf("task: %s %v", data, err)
	}
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(w.Dir(), "result.json"))
	if err != nil || !strings.Contains(string(data), `"route_note": "backend stub was named for this run"`) {
		t.Fatalf("result: %s %v", data, err)
	}
	if _, err := Load(w.ID()); err != nil {
		t.Fatal(err)
	}
	oldID := "20261014-120000-abcdee"
	oldDir := filepath.Join(Root(), oldID)
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDir, "task.json"), []byte(`{"id":"`+oldID+`","backend":"stub","task":{"prompt":"old"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if rec, err := Load(oldID); err != nil || rec.RouteNote != "" {
		t.Fatalf("legacy record=%+v err=%v", rec, err)
	}
}

func TestRecordsKeepContextAndStillLoadResultsWithoutIt(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, n := range []int{285000, 0} {
		w, err := Create("stub", agent.Task{Prompt: "p"})
		if err != nil {
			t.Fatal(err)
		}
		var res agent.Result
		data := `{"status":"ok","turns":1}`
		if n > 0 {
			data = fmt.Sprintf(`{"status":"ok","turns":1,"context_tokens":%d}`, n)
		}
		if err := json.Unmarshal([]byte(data), &res); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(res); err != nil {
			t.Fatal(err)
		}
		rec, err := Load(w.ID())
		if err != nil || !rec.Done || rec.Result.Status != agent.StatusOK {
			t.Fatalf("record = %+v, error = %v", rec, err)
		}
		encoded, err := json.Marshal(rec.Result)
		if err != nil {
			t.Fatal(err)
		}
		if n > 0 && !strings.Contains(string(encoded), `"context_tokens":285000`) {
			t.Errorf("context lost: %s", encoded)
		}
		if n == 0 && strings.Contains(string(encoded), "context_tokens") {
			t.Errorf("old record gained context: %s", encoded)
		}
	}
}

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
	if err != nil || !rec.Done || !strings.Contains(rec.RecordError, recordErr.Error()) || !strings.Contains(rec.RecordError, "close ") || rec.Result.Status != agent.StatusOK {
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
	if err := w.Err(); err == nil || !strings.Contains(err.Error(), "1 event was not recorded") {
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
	if err := w.Err(); err == nil || !strings.Contains(err.Error(), "1 event was not recorded") {
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
	for _, want := range []string{"1 event was not recorded", "close ", resultPath} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Close = %v, missing %q", err, want)
		}
	}
}

// shortWriter leaves a fragment of its first event on disk, then recovers.
type shortWriter struct {
	io.WriteCloser
	failed bool
	err    error
}

func (w *shortWriter) Write(p []byte) (int, error) {
	if !w.failed {
		w.failed = true
		n, err := w.WriteCloser.Write(p[:len(p)/2])
		if err != nil {
			return n, err
		}
		return n, w.err
	}
	return w.WriteCloser.Write(p)
}

func TestWriterKeepsTheNextEventReadableAfterAShortWrite(t *testing.T) {
	for _, cause := range []error{errors.New("disk full"), nil} {
		t.Run(fmt.Sprint(cause), func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			w, err := Create("stub", agent.Task{Prompt: "p"})
			if err != nil {
				t.Fatal(err)
			}
			w.events = &shortWriter{WriteCloser: w.events, err: cause}
			t.Cleanup(func() { _ = w.events.Close() })
			w.Event(agent.Event{Kind: agent.EventText, Text: "lost"})
			w.Event(agent.Event{Kind: agent.EventText, Text: "saved"})
			wantCause := cause
			if wantCause == nil {
				wantCause = io.ErrShortWrite
			}
			if err := w.Err(); err == nil || !strings.Contains(err.Error(), "1 event was not recorded") || !errors.Is(err, wantCause) {
				t.Errorf("Err = %v, want exactly one loss wrapping %v", err, wantCause)
			}
			if err := w.Close(agent.Result{Status: agent.StatusOK}); err == nil {
				t.Error("Close must report the lost event")
			}
			events, err := Events(w.ID())
			if err != nil || len(events) != 1 || events[0].Text != "saved" {
				t.Fatalf("second event must remain readable: events=%+v err=%v", events, err)
			}
		})
	}
}

func TestWriterCountsEventsAfterCloseWithoutChangingTheSavedRecord(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := Create("stub", agent.Task{Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	w.Event(agent.Event{Kind: agent.EventText, Text: "saved"})
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(w.Dir(), "result.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	w.Event(agent.Event{Kind: agent.EventText, Text: "late"})
	w.Event(agent.Event{Kind: agent.EventText, Text: "later"})
	if err := w.Err(); err == nil || !strings.Contains(err.Error(), "2 events were not recorded") || !errors.Is(err, os.ErrClosed) {
		t.Errorf("late events must count as lost: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatalf("late events changed the saved record: err=%v", err)
	}
}

func TestWriterErrCanRaceCloseAndReportsTheCloseError(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := Create("stub", agent.Task{Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.events.Close(); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			<-start
			for range 100 {
				if err := w.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
					t.Errorf("Err = %v, want nil or os.ErrClosed", err)
				}
			}
		})
	}
	close(start)
	closeErr := w.Close(agent.Result{Status: agent.StatusOK})
	wg.Wait()
	if !errors.Is(closeErr, os.ErrClosed) || !errors.Is(w.Err(), os.ErrClosed) {
		t.Errorf("Close = %v, Err = %v, want the close error in both", closeErr, w.Err())
	}
	rec, err := Load(w.ID())
	if err != nil || !rec.Done || !strings.Contains(rec.RecordError, "close ") || strings.Contains(rec.RecordError, "not recorded") {
		t.Fatalf("close-only failure must be saved without inventing lost events: %+v err=%v", rec, err)
	}
}

func TestEventsStillRejectsMalformedUnmarkedRecords(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := Create("stub", agent.Task{Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	write(t, w.Dir(), "events.jsonl", "{broken\n{\"kind\":\"text\",\"text\":\"saved\"}\n")
	if _, err := Events(w.ID()); err == nil {
		t.Fatal("unmarked corruption must still return an error")
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

// scriptedWriter answers each Write with the next step: how many bytes of it reach the file and
// which error comes back. After the script it writes normally.
type scriptedWriter struct {
	io.WriteCloser
	steps []writeStep
}

type writeStep struct {
	n   int // bytes written; -1 means all but the last byte
	err error
}

func (w *scriptedWriter) Write(p []byte) (int, error) {
	if len(w.steps) == 0 {
		return w.WriteCloser.Write(p)
	}
	step := w.steps[0]
	w.steps = w.steps[1:]
	n := step.n
	if n < 0 {
		n = len(p) - 1
	}
	written, err := w.WriteCloser.Write(p[:n])
	if err != nil {
		return written, err
	}
	return written, step.err
}

func TestWriterKeepsEventsApartAcrossConsecutiveFailures(t *testing.T) {
	full := errors.New("disk full")
	for _, tc := range []struct {
		name  string
		steps []writeStep
	}{
		// After a short write, only the separating newline gets out before the next failure.
		{"separator only", []writeStep{{n: 3, err: full}, {n: 1, err: full}}},
		// After a short write, the next write gets nothing out; the one after must still separate.
		{"nothing written", []writeStep{{n: 3, err: full}, {n: 0, err: full}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			w, err := Create("stub", agent.Task{Prompt: "p"})
			if err != nil {
				t.Fatal(err)
			}
			w.events = &scriptedWriter{WriteCloser: w.events, steps: tc.steps}
			w.Event(agent.Event{Kind: agent.EventText, Text: "lost one"})
			w.Event(agent.Event{Kind: agent.EventText, Text: "lost two"})
			w.Event(agent.Event{Kind: agent.EventText, Text: "saved"})
			if err := w.Err(); err == nil || !strings.Contains(err.Error(), "2 events were not recorded") {
				t.Errorf("Err = %v, want two losses", err)
			}
			_ = w.Close(agent.Result{Status: agent.StatusOK})
			events, err := Events(w.ID())
			if err != nil || len(events) != 1 || events[0].Text != "saved" {
				t.Fatalf("only the third event must read back, intact: events=%+v err=%v", events, err)
			}
		})
	}
}

func TestCloseMessageTellsAnIncompleteRecordFromAnUnsavedOne(t *testing.T) {
	incomplete := errors.New("1 event was not recorded: disk full")
	unsaved := errors.Join(incomplete, &ResultWriteError{Err: errors.New("write result.json: disk full")})
	if got := CloseMessage(incomplete); !strings.HasPrefix(got, "run record incomplete: ") {
		t.Errorf("CloseMessage(incomplete) = %q", got)
	}
	if got := CloseMessage(unsaved); !strings.HasPrefix(got, "saving run: ") || !strings.Contains(got, "result.json") {
		t.Errorf("CloseMessage(unsaved) = %q", got)
	}
	if ResultNotSaved(incomplete) || !ResultNotSaved(unsaved) || ResultNotSaved(nil) {
		t.Error("ResultNotSaved must be true only when result.json was not written")
	}
}

func TestCreateLeavesNoRecordWhenItFailsHalfway(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	old := createEventsFile
	createEventsFile = func(string) (io.WriteCloser, error) { return nil, errors.New("disk full") }
	t.Cleanup(func() { createEventsFile = old })

	if _, err := Create("stub", agent.Task{Prompt: "p"}); err == nil {
		t.Fatal("Create succeeded although the events file could not be created")
	}
	entries, err := os.ReadDir(Root())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a failed Create left %d run directories behind", len(entries))
	}
}
