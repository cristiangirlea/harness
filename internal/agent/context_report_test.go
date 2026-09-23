package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withDelta swaps a turn's message_delta for one carrying extra fields.
func withDelta(turn []string, delta string) []string {
	out := append([]string(nil), turn...)
	for i, ev := range out {
		if strings.Contains(ev, `"type":"message_delta"`) {
			out[i] = delta
		}
	}
	return out
}

func TestAPIRecordsWhatTheServerClearedVerbatim(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The report's shape is not documented here, so the test uses one at
	// the top level of message_delta, one inside its delta, and a null.
	turns := [][]string{
		withDelta(toolTurn("t1", "read_file", `{"path":"a.txt"}`),
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"context_management":{"applied_edits":[{"type":"clear_tool_uses_20250919","cleared_tool_uses":3}]},"usage":{"output_tokens":20}}`),
		withDelta(toolTurn("t2", "read_file", `{"path":"a.txt"}`),
			`{"type":"message_delta","delta":{"stop_reason":"tool_use","context_management":{"note":"inside the delta"}},"usage":{"output_tokens":20}}`),
		withDelta(textTurn("done"),
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"context_management":null,"usage":{"output_tokens":20}}`),
	}
	res, events, fake := runFake(t, turns, Task{Prompt: "read it", Dir: dir, Options: Options{Mode: ModeReadOnly}})
	if res.Status != StatusOK {
		t.Fatalf("result = %+v", res)
	}
	var reports []string
	for _, e := range events {
		if e.Kind == EventInfo && strings.HasPrefix(e.Text, "server context management") {
			reports = append(reports, e.Text)
		}
	}
	if len(reports) != 2 {
		t.Fatalf("reports = %q, want one per turn that carried one and none for the null", reports)
	}
	if !strings.Contains(reports[0], "turn 1:") || !strings.Contains(reports[0], `{"applied_edits":[{"type":"clear_tool_uses_20250919","cleared_tool_uses":3}]}`) {
		t.Errorf("first report = %q, want the turn and the JSON exactly as sent", reports[0])
	}
	if !strings.Contains(reports[1], "turn 2:") || !strings.Contains(reports[1], "inside the delta") {
		t.Errorf("second report = %q", reports[1])
	}
	// Recording it must not change what goes back: history stays append-only.
	for i, req := range fake.requests {
		if raw, _ := json.Marshal(req["messages"]); strings.Contains(string(raw), "applied_edits") {
			t.Errorf("request %d echoes the server's report back in the history", i+1)
		}
	}
}

func TestPresent(t *testing.T) {
	for raw, want := range map[string]bool{"": false, "null": false, " {} ": false, "[]": false, `{"a":1}`: true, "0": true} {
		if got := present([]byte(raw)); got != want {
			t.Errorf("present(%q) = %v, want %v", raw, got, want)
		}
	}
}
