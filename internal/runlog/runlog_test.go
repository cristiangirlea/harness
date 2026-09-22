package runlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
)

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
	t.Setenv("HX_HOME", t.TempDir())
	task := agent.Task{Prompt: "p", Dir: t.TempDir(), Mode: agent.ModeAsk}
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
	t.Setenv("HX_HOME", t.TempDir())
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
	t.Setenv("HX_HOME", t.TempDir())
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
