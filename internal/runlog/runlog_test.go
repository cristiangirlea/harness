package runlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
)

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

	if _, _, err := LoadConversation("../escape"); err == nil {
		t.Error("a path-like id must be refused")
	}
}
