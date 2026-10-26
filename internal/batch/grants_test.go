package batch

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/flow"
	"github.com/cristiangirlea/harness/internal/owner"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestTimelineViewerProjectGrant(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:me")
	project := t.TempDir()
	w, err := runlog.Create("fake", agent.Task{Dir: filepath.Join(project, ".hrn", "worktrees", "task")})
	if err != nil {
		t.Fatal(err)
	}
	w.Event(agent.Event{Kind: agent.EventToolCall, Tool: "shell", Text: "granted tool"})
	w.Event(agent.Event{Kind: agent.EventToolResult, Tool: "shell", Text: "ok"})
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	res := Result{ID: "20260101-000000-abcdef", Dir: project, Started: time.Now(), Tasks: []TaskResult{{ID: "task", Attempts: []Attempt{{RunID: w.ID()}}}}}
	if err := save(res); err != nil {
		t.Fatal(err)
	}
	v := owner.Viewer{ID: "user:reader", Grants: []owner.Grant{{Viewer: "user:reader", Project: project}}}
	if _, err := LoadResultFor(v, res.ID); err != nil {
		t.Fatal(err)
	}
	if got, _, err := ListFor(v); err != nil || len(got) != 1 {
		t.Fatalf("list = %v, %v", got, err)
	}
	events, err := Timeline(v, res.ID)
	if err != nil {
		t.Fatal(err)
	}
	if node := find(flow.Build(events), AttemptStep("task", 1)+"/run"); node == nil || len(node.Children) != 1 {
		t.Fatalf("granted run = %+v", node)
	}
	v.Grants[0].Project = project + "2"
	if _, err := LoadResultFor(v, res.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("foreign load: %v", err)
	}
	if got, _, err := ListFor(v); err != nil || len(got) != 0 {
		t.Fatalf("foreign list = %v, %v", got, err)
	}
	if _, err := Timeline(v, res.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("foreign timeline: %v", err)
	}
}

func TestTimelineViewerGrantDoesNotExposeAnotherProjectsRun(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:me")
	project := t.TempDir()
	w, err := runlog.Create("fake", agent.Task{Dir: t.TempDir(), Prompt: "private child"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	res := Result{ID: "20260101-000000-abcdef", Dir: project, Started: time.Now(), Tasks: []TaskResult{{ID: "task", Attempts: []Attempt{{RunID: w.ID()}}}}}
	if err := save(res); err != nil {
		t.Fatal(err)
	}
	v := owner.Viewer{ID: "user:reader", Grants: []owner.Grant{{Viewer: "user:reader", Project: project}}}
	for _, live := range []bool{false, true} {
		if live {
			fw, err := flow.Open(FlowPath(res.ID), flow.Event{Batch: res.ID})
			if err != nil {
				t.Fatal(err)
			}
			fw.Emit(flow.Event{Step: BatchStep, Kind: flow.KindBatch})
			fw.Emit(flow.Event{Step: AttemptStep("task", 1) + "/run", Parent: AttemptStep("task", 1), Kind: flow.KindRun, Run: w.ID(), Text: "private child"})
			fw.Close()
		}
		events, err := Timeline(v, res.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Text == "private child" {
				t.Fatalf("live=%v: unreadable child leaked", live)
			}
		}
		if node := find(flow.Build(events), AttemptStep("task", 1)+"/run"); node == nil || node.Kind != flow.KindError {
			t.Fatalf("live=%v: unreadable child must be a gap: %+v", live, node)
		}
	}
}
