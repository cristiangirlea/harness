package agent

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/cristiangirlea/harness/internal/kernel"
)

var claudeToolStatsLines = []string{
	`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"edit-1","name":"edit_file","input":{}}]}}`,
	`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"read-1","name":"read_file","input":{}}]}}`,
	`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"edit-1","content":"bad","is_error":true}]}}`,
	`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"read-1","content":"ok"}]}}`,
	`{"type":"result","subtype":"success","result":"done"}`,
}

func TestClaudeToolResultsKeepTheNameOfTheirToolUse(t *testing.T) {
	parse := newClaudeParse()
	var res Result
	var results []Event
	emit := func(e Event) {
		if e.Kind == EventToolResult {
			results = append(results, e)
		}
	}
	for _, line := range claudeToolStatsLines {
		if !parse([]byte(line), emit, &res) {
			t.Fatalf("unparsed line: %s", line)
		}
	}
	parse(nil, emit, &res)
	if len(results) != 2 || results[0].Tool != "edit_file" || !results[0].IsError || results[0].Text != "bad" || results[1].Tool != "read_file" {
		t.Fatalf("results = %+v", results)
	}
	unknown := []byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"unknown","content":"?"}]}}`)
	parse(unknown, emit, &res)
	parseClaudeStream([]byte(claudeToolStatsLines[2]), emit, &res)
	if len(results) != 4 || results[2].Tool != "" || results[3].Tool != "" {
		t.Fatalf("results = %+v", results)
	}
}

func TestHelperClaudeToolStats(t *testing.T) {
	if os.Getenv("HRN_CLAUDE_TOOL_STATS_HELPER") != "1" {
		return
	}
	for _, line := range claudeToolStatsLines {
		fmt.Println(line)
	}
	os.Exit(0)
}

func TestRunCountsClaudeToolResultsUnderTheirToolNames(t *testing.T) {
	t.Setenv("HRN_CLAUDE_TOOL_STATS_HELPER", "1")
	b := NewClaudeBackend()
	b.name, b.candidates = os.Args[0], nil
	b.args = func(Task, string) []string { return []string{"-test.run=^TestHelperClaudeToolStats$", "--"} }
	r := Run(context.Background(), b, Task{Prompt: "test", Dir: t.TempDir()}, func(Event) {})
	if r.Status != StatusOK || r.ToolCalls != 2 || len(r.ToolStats) != 2 || r.ToolStats["edit_file"] != (kernel.ToolStat{Calls: 1, Errors: 1, ResultBytes: 3}) || r.ToolStats["read_file"] != (kernel.ToolStat{Calls: 1, ResultBytes: 2}) {
		t.Fatalf("result = %+v", r)
	}
}

func TestCodexCommandResultsKeepTheShellToolName(t *testing.T) {
	var res Result
	var events []Event
	for _, line := range []string{
		`{"type":"item.started","item":{"type":"command_execution","command":"test"}}`,
		`{"type":"item.completed","item":{"type":"command_execution","aggregated_output":"bad","exit_code":1}}`,
	} {
		if !parseCodexStream([]byte(line), func(e Event) { events = append(events, e) }, &res) {
			t.Fatalf("unparsed line: %s", line)
		}
	}
	if len(events) != 2 || events[0].Tool != "shell" || events[1].Kind != EventToolResult || events[1].Tool != "shell" || !events[1].IsError || events[1].Text != "bad" {
		t.Fatalf("events = %+v", events)
	}
}
