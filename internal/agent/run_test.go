package agent

import (
	"context"
	"reflect"
	"testing"

	"github.com/cristiangirlea/harness/internal/kernel"
)

type emitting struct{ events []Event }

func (e emitting) Name() string            { return "emitting" }
func (e emitting) Detect() (string, error) { return "stub", nil }
func (e emitting) Run(_ context.Context, _ Task, emit func(Event)) Result {
	for _, ev := range e.events {
		emit(ev)
	}
	return Result{Status: StatusOK, ToolCalls: 99}
}

func TestRunCountsCallsErrorsAndResultBytesPerTool(t *testing.T) {
	events := []Event{
		{Kind: EventToolCall, Tool: "read_file", Partial: true},
		{Kind: EventToolCall, Tool: "read_file"},
		{Kind: EventToolResult, Tool: "read_file", Text: "é"},
		{Kind: EventToolCall, Tool: "edit_file"},
		{Kind: EventToolResult, Tool: "edit_file", Text: "bad", IsError: true},
		{Kind: EventToolCall},
		{Kind: EventToolResult, Text: "?", IsError: true},
		{Kind: EventToolResult, Tool: "result_only", Text: "ok"},
		{Kind: EventText, Text: "ignored"},
	}
	seen := 0
	r := Run(context.Background(), emitting{events}, Task{}, func(Event) { seen++ })
	want := map[string]kernel.ToolStat{
		"read_file":   {Calls: 1, ResultBytes: 2},
		"edit_file":   {Calls: 1, Errors: 1, ResultBytes: 3},
		"":            {Calls: 1, Errors: 1, ResultBytes: 1},
		"result_only": {ResultBytes: 2},
	}
	if !reflect.DeepEqual(r.ToolStats, want) || r.ToolCalls != 3 || seen != len(events) {
		t.Fatalf("result = %+v, events = %d", r, seen)
	}
}

func TestDurableRunsDoNotRecordEventToolStats(t *testing.T) {
	r := Run(context.Background(), emitting{[]Event{
		{Kind: EventToolCall, Tool: "read_file"},
		{Kind: EventToolResult, Tool: "read_file", Text: "bad", IsError: true},
	}}, Task{Durable: &DurableExecution{}}, func(Event) {})
	if r.ToolStats != nil || r.ToolCalls != 99 {
		t.Fatalf("result = %+v", r)
	}
}

func TestRunCountsToolCalls(t *testing.T) {
	b := emitting{[]Event{
		{Kind: EventToolCall, Tool: "grep"},
		{Kind: EventToolResult, Tool: "grep"},
		{Kind: EventToolCall, Tool: "read_file", Partial: true}, // a fragment, not a call
		{Kind: EventToolCall, Tool: "read_file"},
		{Kind: EventText, Text: "done"},
	}}
	var seen int
	res := Run(context.Background(), b, Task{}, func(Event) { seen++ })
	if res.ToolCalls != 2 {
		t.Errorf("ToolCalls = %d, want 2: whole calls only, and Run's count replaces the backend's", res.ToolCalls)
	}
	if seen != 5 {
		t.Errorf("emit saw %d events, want every one of the 5 passed through", seen)
	}
}
