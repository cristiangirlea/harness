package runlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
)

func TestARunRecordsTheSetupItStartedWith(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "AGENTS.md"), []byte("Rules of the repo.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := Create("api", agent.Task{Prompt: "Fix it.", Dir: ws})
	if err != nil {
		t.Fatal(err)
	}
	// A later edit is not what the run started with.
	if err := os.WriteFile(filepath.Join(ws, "AGENTS.md"), []byte("Edited by the agent.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	rec, err := Load(w.ID())
	if err != nil || rec.Setup == nil {
		t.Fatalf("record = %+v, error = %v", rec, err)
	}
	if rec.Setup.Files["AGENTS.md"] == "" || rec.Setup.Digest == "" {
		t.Fatalf("setup = %+v", rec.Setup)
	}
	again := agent.ResolveSetup("api", agent.Task{Dir: ws})
	if again.Digest == rec.Setup.Digest {
		t.Fatalf("the saved digest follows the edited file")
	}
	data, err := os.ReadFile(filepath.Join(w.Dir(), "result.json"))
	if err != nil || strings.Contains(string(data), "Rules of the repo") {
		t.Fatalf("result.json holds the file's text or is unreadable: %v", err)
	}
}

func TestARecordWithoutASetupStillLoads(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	id := "20261014-120000-abcdef"
	dir := filepath.Join(Root(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := `{"id":"` + id + `","backend":"api","task":{"prompt":"p","dir":"."},"result":{"status":"ok"}}`
	if err := os.WriteFile(filepath.Join(dir, "result.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, err := Load(id)
	if err != nil || rec.Setup != nil || rec.Task.Prompt != "p" {
		t.Fatalf("old record = %+v, error = %v", rec, err)
	}
}
