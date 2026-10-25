package runlog

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/owner"
)

func TestNewRunsRecordTheLocalOwner(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:me")
	w, err := Create("claude", agent.Task{Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	w.Close(agent.Result{Status: agent.StatusOK})
	rec, err := Load(w.ID())
	if err != nil || rec.Owner != "local:me" {
		t.Fatalf("owner = %q (%v), want local:me", rec.Owner, err)
	}
}

func TestOnlyTheOwnerReadsARunAndLegacyRunsBelongToTheLocalUser(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:me")
	legacy := filepath.Join(Root(), "20200101-000000-aaaaaa")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"id": "x", "backend": "claude", "task": map[string]any{"prompt": "old"}})
	if err := os.WriteFile(filepath.Join(legacy, "result.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := Create("codex", agent.Task{Prompt: "mine"})
	if err != nil {
		t.Fatal(err)
	}
	w.Close(agent.Result{})

	me, stranger := owner.LocalViewer(), owner.Viewer{ID: owner.Email("x@example.com")}
	if _, err := LoadFor(me, filepath.Base(legacy)); err != nil {
		t.Fatalf("the local user must read a run written before ownership: %v", err)
	}
	if _, err := LoadFor(stranger, w.ID()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("another viewer got %v, want a not-found error that does not confirm the run exists", err)
	}
	if recs, _ := ListFor(stranger, 10); len(recs) != 0 {
		t.Fatalf("another viewer listed %d runs", len(recs))
	}
	if recs, _ := ListFor(me, 1); len(recs) != 1 {
		t.Fatalf("limit applies to readable runs, got %d", len(recs))
	}
	if recs, _ := ListFor(me, 10); len(recs) != 2 {
		t.Fatalf("the owner lists %d runs, want 2", len(recs))
	}
}
