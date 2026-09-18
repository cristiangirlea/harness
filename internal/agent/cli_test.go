package agent

import (
	"strings"
	"testing"
)

func TestParseClaudeStream(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"init","model":"claude-opus-5"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Looking."},{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"a.go"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"package a"}]}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"num_turns":2,"result":"Done.","total_cost_usd":0.0123,"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100}}`,
		`not json`,
	}
	var res Result
	var events []Event
	emit := func(e Event) { events = append(events, e) }
	unparsed := 0
	for _, l := range lines {
		if !parseClaudeStream([]byte(l), emit, &res) {
			unparsed++
		}
	}
	if unparsed != 1 {
		t.Errorf("unparsed = %d, want 1", unparsed)
	}
	if res.Status != StatusOK || res.Final != "Done." || res.Turns != 2 || res.CostUSD != 0.0123 || res.CacheRead != 100 || res.Model != "claude-opus-5" {
		t.Errorf("unexpected result %+v", res)
	}
	if len(events) != 3 || events[1].Tool != "Read" || events[1].Text != "a.go" || events[2].Text != "package a" {
		t.Errorf("unexpected events %+v", events)
	}
}

func TestClaudeArgsModeMapping(t *testing.T) {
	b := NewClaudeBackend()
	args := strings.Join(b.args(Task{Mode: ModeAuto, Model: "sonnet", MaxTurns: 5}, ""), " ")
	for _, want := range []string{"-p", "stream-json", "--permission-mode acceptEdits", "--model sonnet", "--max-turns 5"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q missing %q", args, want)
		}
	}
}

func TestVersionLess(t *testing.T) {
	if !versionLess("2.1.9", "2.1.270") || versionLess("2.1.271", "2.1.270") {
		t.Error("versionLess compares numerically")
	}
}
