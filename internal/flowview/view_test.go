package flowview

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/flow"
	"github.com/cristiangirlea/harness/internal/owner"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestFilterAndWriter(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	root := []*flow.Node{{Kind: flow.KindRun, Run: "r", Status: flow.StatusOK, Children: []*flow.Node{{Kind: flow.KindTool, Name: "shell", Reason: "failed"}}}}
	if got := Filter(root, "simple"); len(got) != 1 || len(got[0].Children) != 0 {
		t.Fatalf("simple tree = %#v", got)
	}
	var b strings.Builder
	if err := WriteText(&b, RunSummary(root), Filter(root, "technical"), "technical"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "why: failed") {
		t.Fatalf("text = %q", b.String())
	}
}

func TestLoadRunAndBatchSummaries(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:one")
	w, err := runlog.Create("stub", agent.Task{Prompt: "run prompt"})
	if err != nil {
		t.Fatal(err)
	}
	w.Event(agent.Event{Kind: agent.EventToolCall, Tool: "shell", Text: "go test"})
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	got, err := Load(owner.LocalViewer(), w.ID())
	if err != nil || !strings.Contains(got.Summary, "run "+w.ID()+": ok") || len(got.Events) == 0 {
		t.Fatalf("run view = %+v err=%v", got, err)
	}
	batchID := "20261021-120000-abcdef"
	res := batch.Result{ID: batchID, Owner: "local:one", Started: time.Now(), Tasks: []batch.TaskResult{{ID: "task", Status: batch.StatusFail, Attempts: []batch.Attempt{{Gates: []batch.GateResult{{Command: "go test ./...", ExitCode: 1}}}}}}}
	if err := batch.SaveResult(res); err != nil {
		t.Fatal(err)
	}
	got, err = Load(owner.LocalViewer(), batchID)
	if err != nil || !strings.Contains(got.Summary, "gate go test ./...: exited 1") || len(got.Tree) == 0 {
		t.Fatalf("batch view = %+v err=%v", got, err)
	}
	if _, err := Load(owner.Viewer{ID: "local:two"}, w.ID()); err == nil {
		t.Fatal("loaded another owner's run")
	}
}

func TestLoadCleansStoredFlowAgain(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:one")
	id := "20261021-120000-abcdef"
	if err := batch.SaveResult(batch.Result{ID: id, Owner: "local:one", Started: time.Now()}); err != nil {
		t.Fatal(err)
	}
	line := `{"v":1,"seq":1,"step":"batch","kind":"batch","text":"ghp_abcdefghijklmnop1234"}` + "\n"
	if err := os.MkdirAll(filepath.Dir(batch.FlowPath(id)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(batch.FlowPath(id), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(owner.LocalViewer(), id)
	if err != nil || strings.Contains(fmt.Sprint(got.Tree), "ghp_abcdefghijklmnop1234") {
		t.Fatalf("view = %+v err=%v", got, err)
	}
}
