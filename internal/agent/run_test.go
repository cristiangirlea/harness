package agent

import (
	"context"
	"testing"
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
