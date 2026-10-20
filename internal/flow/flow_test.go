package flow

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestWriterNumbersEventsAcrossReopeningAndReadReplaysThemInOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.flow.jsonl")
	w, err := Open(path, Event{Owner: "local:me", Batch: "b1"})
	if err != nil {
		t.Fatal(err)
	}
	w.Emit(Event{Step: "batch", Kind: KindBatch, Status: StatusStarted})
	w.Emit(Event{Step: "task:a", Parent: "batch", Kind: KindTask, Status: StatusStarted})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w, err = Open(path, Event{Owner: "local:me", Batch: "b1"})
	if err != nil {
		t.Fatal(err)
	}
	w.Emit(Event{Step: "task:a", Kind: KindTask, Status: StatusOK})
	w.Close()

	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("read %d events, want 3", len(got))
	}
	for i, e := range got {
		if e.Seq != int64(i+1) || e.V != Version || e.Owner != "local:me" || e.Batch != "b1" || e.Time.IsZero() {
			t.Fatalf("event %d = %+v: want seq %d with version, owner, batch and time stamped", i, e, i+1)
		}
	}
}

func TestReadSkipsATornLineDuplicatesAndNewerVersions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.jsonl")
	body := `{"v":1,"seq":2,"step":"b","kind":"task"}
{"v":1,"seq":1,"step":"a","kind":"batch"}
{"v":1,"seq":2,"step":"dup","kind":"task"}
{"v":99,"seq":3,"step":"future","kind":"task"}
{"v":1,"seq":4,"step":"c","ki`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	for _, e := range got {
		steps = append(steps, e.Step)
	}
	if strings.Join(steps, ",") != "a,b" {
		t.Fatalf("steps = %v, want a,b: sorted by seq, first of a duplicate kept, torn and unknown versions dropped", steps)
	}
}

func TestParallelEmittersGetDistinctSequenceNumbers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.jsonl")
	w, err := Open(path, Event{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				w.Emit(Event{Step: fmt.Sprintf("task:%d/tool%d", g, i), Kind: KindTool})
			}
		}()
	}
	wg.Wait()
	w.Close()
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 200 {
		t.Fatalf("read %d events, want all 200 without loss or collision", len(got))
	}
}

func TestEmitMasksSecretsBeforeTheyReachDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	w, _ := Open(path, Event{})
	w.Emit(Event{Step: "g", Kind: KindGate, Name: "curl -H 'Authorization: Bearer abcdefghijklmnop'", Text: "API_TOKEN=supersecretvalue123"})
	w.Close()
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "abcdefghijklmnop") || strings.Contains(string(data), "supersecretvalue123") {
		t.Fatalf("a secret reached the flow file: %s", data)
	}
}

func TestBuildFoldsUpdatesIntoOneStepAndKeepsChildrenInOrder(t *testing.T) {
	t0 := time.Unix(1000, 0)
	nodes := Build([]Event{
		{Seq: 1, Time: t0, Step: "batch", Kind: KindBatch, Status: StatusStarted},
		{Seq: 2, Time: t0, Step: "task:a", Parent: "batch", Kind: KindTask, Status: StatusStarted, Next: "runs claude"},
		{Seq: 3, Time: t0, Step: "task:b", Parent: "batch", Kind: KindTask, Status: StatusWaiting},
		{Seq: 4, Time: t0.Add(time.Minute), Step: "task:a", Kind: KindTask, Status: StatusFailed, Reason: "gate go test exited 1"},
	})
	if len(nodes) != 1 || len(nodes[0].Children) != 2 {
		t.Fatalf("want one root with two tasks, got %+v", nodes)
	}
	a := nodes[0].Children[0]
	if a.Step != "task:a" || a.Status != StatusFailed || a.Reason == "" || a.Next != "" || !a.Ended.Equal(t0.Add(time.Minute)) {
		t.Fatalf("task a = %+v: want the failure folded in, its next step cleared and its end time set", a)
	}
}

func TestBuildShowsAStepWhoseParentIsMissingOrLoopsAsAGap(t *testing.T) {
	nodes := Build([]Event{
		{Seq: 1, Step: "x", Parent: "gone", Kind: KindRun},
		{Seq: 2, Step: "p", Parent: "q", Kind: KindTask},
		{Seq: 3, Step: "q", Parent: "p", Kind: KindTask},
	})
	n := 0
	Walk(nodes, func(*Node, int) { n++ })
	if n != 3 || !nodes[0].Orphan {
		t.Fatalf("every step must stay visible and the orphan marked, got %d nodes %+v", n, nodes)
	}
}

func TestFromRunPairsToolCallsWithResultsAndRecordsTheRoute(t *testing.T) {
	t0 := time.Unix(2000, 0)
	rec := runlog.Record{ID: "r1", Owner: "local:me", Backend: "codex", Started: t0, Done: true,
		RoutedBy: &runlog.RoutedBy{Category: "implement", Declared: true, Set: "hrn route set"},
		Task:     agent.Task{Prompt: "fix the bug\nmore detail"},
		Result:   agent.Result{Status: agent.StatusError, Error: "gave up", Turns: 3, ToolCalls: 3, Duration: time.Minute}}
	events := []agent.Event{
		{Time: t0, Kind: agent.EventThinking, Text: "private musing"},
		{Time: t0, Kind: agent.EventToolCall, Tool: "shell", Text: "go test ./..."},
		{Time: t0, Kind: agent.EventToolCall, Tool: "read", Text: "a.go"},
		{Time: t0, Kind: agent.EventToolResult, Tool: "shell", Text: "FAIL", IsError: true},
		{Time: t0, Kind: agent.EventToolResult, Tool: "read", Text: "package a"},
		{Time: t0, Kind: agent.EventToolCall, Tool: "edit", Text: "a.go"},
		{Time: t0, Kind: agent.EventError, Text: "stream broke"},
	}
	nodes := Build(FromRun(rec, events, "task:t/a1"))
	if len(nodes) != 1 || !nodes[0].Orphan || nodes[0].Step != "task:t/a1/run" {
		t.Fatalf("the run must hang under its attempt step, got %+v", nodes)
	}
	run := nodes[0]
	if run.Status != StatusFailed || !strings.Contains(run.Reason, "implement (declared)") {
		t.Fatalf("run = %+v: want failed with the recorded route reason", run)
	}
	want := map[string]Status{"shell": StatusFailed, "read": StatusOK, "edit": StatusSkipped}
	tools := 0
	for _, c := range run.Children {
		if c.Kind == KindTool {
			tools++
			if c.Status != want[c.Name] {
				t.Errorf("tool %s = %s, want %s", c.Name, c.Status, want[c.Name])
			}
		}
		if strings.Contains(c.Text, "private musing") {
			t.Fatal("thinking must not become a step")
		}
	}
	if tools != 3 {
		t.Fatalf("want 3 tool steps, got %d", tools)
	}
}

func TestFromRunOfALegacyUnfinishedRunStaysRunning(t *testing.T) {
	rec := runlog.Record{ID: "old", Backend: "claude", Started: time.Unix(3000, 0)}
	events := []agent.Event{{Kind: agent.EventToolCall, Tool: "Bash"}, {Kind: agent.EventToolResult, Text: "ok"}}
	nodes := Build(FromRun(rec, events, ""))
	if nodes[0].Step != "run:old" || nodes[0].Status != StatusStarted || nodes[0].Children[0].Status != StatusOK {
		t.Fatalf("got %+v: a run without result is still going, and an unnamed result closes the oldest call", nodes[0])
	}
}

func TestFromRunMasksSecretsInToolText(t *testing.T) {
	rec := runlog.Record{ID: "r", Started: time.Unix(1, 0)}
	cut := strings.Repeat("x", 170) + " git clone https://bob:hunter2secret@example.com/repo"
	events := []agent.Event{{Kind: agent.EventToolCall, Tool: "shell", Text: "export API_TOKEN=supersecretvalue123"},
		{Kind: agent.EventToolCall, Tool: "shell", Text: cut}}
	for _, e := range FromRun(rec, events, "") {
		if strings.Contains(e.Text, "supersecretvalue123") || strings.Contains(e.Text, "hunter2") {
			t.Fatalf("a derived step kept a secret: %q", e.Text)
		}
	}
}

func TestOpenCreatesTheDirectoryItWritesIn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new", "f.jsonl")
	w, err := Open(path, Event{})
	if err != nil {
		t.Fatal(err)
	}
	w.Emit(Event{Step: "s", Kind: KindInfo})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}
