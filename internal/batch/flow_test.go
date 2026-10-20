package batch

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/flow"
	"github.com/cristiangirlea/harness/internal/owner"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func runFor(t *testing.T, backend string, res agent.Result, events ...agent.Event) string {
	t.Helper()
	w, err := runlog.Create(backend, agent.Task{Prompt: "do it"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		w.Event(e)
	}
	w.Close(res)
	return w.ID()
}

// legacyBatch saves a record as older builds wrote it: no owner and no flow file.
func legacyBatch(t *testing.T) Result {
	t.Helper()
	failed := runFor(t, "codex", agent.Result{Status: agent.StatusOK, Duration: time.Second},
		agent.Event{Kind: agent.EventToolCall, Tool: "shell", Text: "go build"}, agent.Event{Kind: agent.EventToolResult, Tool: "shell", Text: "ok"})
	passed := runFor(t, "claude", agent.Result{Status: agent.StatusOK, Duration: time.Second})
	canceled := runFor(t, "codex", agent.Result{Status: agent.StatusCanceled})
	res := Result{ID: "20260101-000000-abcdef", Started: time.Now().Add(-time.Hour), Base: "0123456789abcdef", Parallel: 2, Duration: time.Minute,
		Tasks: []TaskResult{
			{ID: "fix", Prompt: strings.Repeat("long words ", 40) + "\nmore", Purpose: "debug", Status: StatusPass, Commit: "c0ffee", Duration: time.Minute, Attempts: []Attempt{
				{Backend: "codex", RunID: failed, Result: agent.Result{Status: agent.StatusOK}, Gates: []GateResult{
					{Command: "go vet ./...", ExitCode: 0}, {Command: "go test ./...", ExitCode: 1, Output: "--- FAIL: TestX\nFAIL", Duration: 90 * time.Second}}},
				{Backend: "claude", RunID: passed, Result: agent.Result{Status: agent.StatusOK}, Gates: []GateResult{{Command: "go test ./...", ExitCode: 0}}},
			}},
			{ID: "stop", Prompt: "slow", Status: StatusCanceled, Attempts: []Attempt{
				{Backend: "codex", RunID: canceled, Result: agent.Result{Status: agent.StatusCanceled},
					Gates: []GateResult{{Command: "go test", ExitCode: -1, Killed: KilledCanceled}}}}},
		}}
	if err := save(res); err != nil {
		t.Fatal(err)
	}
	return res
}

func find(nodes []*flow.Node, step string) *flow.Node {
	var got *flow.Node
	flow.Walk(nodes, func(n *flow.Node, _ int) {
		if n.Step == step {
			got = n
		}
	})
	return got
}

func mustTimeline(t *testing.T, id string) []flow.Event {
	t.Helper()
	events, err := Timeline(owner.LocalViewer(), id)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestTimelineRebuildsALegacyBatchWithRetriesGatesAndRuns(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:me")
	res := legacyBatch(t)
	events := mustTimeline(t, res.ID)
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d: the merged timeline needs one sequence", i, e.Seq)
		}
	}
	nodes := flow.Build(events)
	if len(nodes) != 1 || nodes[0].Step != BatchStep || nodes[0].Status != flow.StatusCanceled {
		t.Fatalf("want one canceled batch root, got %+v", nodes)
	}
	gate := find(nodes, GateStep("fix", 1, 2))
	if gate == nil || gate.Status != flow.StatusFailed || gate.Reason != "exited 1" || gate.Text != "FAIL" {
		t.Fatalf("failing gate = %+v", gate)
	}
	if d := gate.Ended.Sub(gate.Started); d != 90*time.Second {
		t.Errorf("rebuilt gate took %s, want its recorded 1m30s", d)
	}
	retry := find(nodes, AttemptStep("fix", 2))
	if retry == nil || retry.Status != flow.StatusOK || !strings.Contains(retry.Reason, "gate go test ./... exited 1") {
		t.Fatalf("retry = %+v: want its recorded reason", retry)
	}
	run := find(nodes, AttemptStep("fix", 1)+"/run")
	if run == nil || len(run.Children) != 1 || run.Children[0].Kind != flow.KindTool {
		t.Fatalf("the first attempt's run and its tool call must hang under the attempt, got %+v", run)
	}
	if fix := find(nodes, TaskStep("fix")); len([]rune(fix.Text)) > 161 {
		t.Errorf("task line is %d runes: a tree line must stay short", len([]rune(fix.Text)))
	}
	if fix := find(nodes, TaskStep("fix")); fix.Status != flow.StatusOK || !strings.Contains(fix.Next, "hrn batch show") {
		t.Fatalf("task = %+v: want ok with what to do next", fix)
	}
	if g := find(nodes, GateStep("stop", 1, 1)); g == nil || g.Status != flow.StatusCanceled {
		t.Fatalf("canceled gate = %+v", g)
	}
}

func TestTimelinePrefersTheLiveFlowAndShowsAMissingRunAsAGap(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	res := legacyBatch(t)
	w, err := flow.Open(FlowPath(res.ID), flow.Event{Batch: res.ID})
	if err != nil {
		t.Fatal(err)
	}
	w.Emit(flow.Event{Step: BatchStep, Kind: flow.KindBatch, Status: flow.StatusStarted, Text: "live"})
	w.Close()
	if err := os.RemoveAll(filepath.Join(runlog.Root(), res.Tasks[0].Attempts[0].RunID)); err != nil {
		t.Fatal(err)
	}

	nodes := flow.Build(mustTimeline(t, res.ID))
	if find(nodes, BatchStep).Text != "live" || find(nodes, TaskStep("fix")) != nil {
		t.Fatal("a batch with live events must not be rebuilt from its record")
	}
	gap := find(nodes, AttemptStep("fix", 1)+"/run")
	if gap == nil || gap.Kind != flow.KindError || !gap.Orphan {
		t.Fatalf("a pruned run must show as a gap, got %+v", gap)
	}
}

func TestAnotherViewerCannotReadOrListABatch(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:me")
	res := legacyBatch(t)
	stranger := owner.Viewer{ID: owner.Email("x@example.com")}
	if _, err := LoadResultFor(stranger, res.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("got %v, want not found", err)
	}
	if _, err := Timeline(stranger, res.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("timeline got %v, want not found", err)
	}
	if got, _, _ := ListFor(stranger); len(got) != 0 {
		t.Fatalf("listed %d batches", len(got))
	}
	if got, _, _ := ListFor(owner.LocalViewer()); len(got) != 1 {
		t.Fatalf("the local user must list the legacy batch, got %d", len(got))
	}
}

func TestRemoveRecordAlsoRemovesTheFlowFile(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	res := legacyBatch(t)
	w, err := flow.Open(FlowPath(res.ID), flow.Event{})
	if err != nil {
		t.Fatal(err)
	}
	w.Emit(flow.Event{Step: BatchStep, Kind: flow.KindBatch})
	w.Close()
	if _, err := RemoveRecord(res.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(FlowPath(res.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("flow file left behind: %v", err)
	}
}

func TestATaskLineIsMaskedBeforeItIsShortened(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	prompt := strings.Repeat("y", 130) + " git clone https://bob:hunter2secret@example.com/repo"
	res := Result{ID: "20260101-000000-cccccc", Started: time.Now(), Tasks: []TaskResult{{ID: "t", Prompt: prompt, Status: StatusPass}}}
	for _, e := range FlowOf(res, nil) {
		if strings.Contains(e.Text, "hunter2") {
			t.Fatalf("the cut kept part of a secret: %q", e.Text)
		}
	}
}
