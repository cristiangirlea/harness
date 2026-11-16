package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The recorded gated run: two exec scripts call run_command and two only read ALL_TOOLS.
const gateListingThread = "01a1e495-3b04-7ee0-ba29-7b4ae5f24d5b"

// shiftRollout moves every time in a recorded rollout by d, keeping their spacing.
func shiftRollout(t *testing.T, text string, d time.Duration) string {
	t.Helper()
	var out []string
	for line := range strings.SplitSeq(strings.TrimSpace(text), "\n") {
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		at, err := time.Parse(time.RFC3339Nano, m["timestamp"].(string))
		if err != nil {
			t.Fatal(err)
		}
		m["timestamp"] = at.Add(d).UTC().Format(time.RFC3339Nano)
		payload := m["payload"].(map[string]any)
		for _, key := range []string{"started_at_ms", "completed_at_ms"} {
			if n, ok := payload[key].(json.Number); ok {
				ms, err := n.Int64()
				if err != nil {
					t.Fatal(err)
				}
				payload[key] = ms + d.Milliseconds()
			}
		}
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(data))
	}
	return strings.Join(out, "\n") + "\n"
}

// placeGateRollout writes the recorded rollout as if its session had started at the given time.
func placeGateRollout(t *testing.T, home string, at time.Time, rewrite func(string) string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "codex-gate-rollout.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "sessions", filepath.FromSlash(time.Now().Format("2006/01/02")))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	recorded := time.Date(2026, 11, 16, 13, 39, 8, 251_000_000, time.UTC)
	text := shiftRollout(t, string(data), at.Sub(recorded))
	name := "rollout-" + time.Now().Format("2006-01-02T15-04-05") + "-" + gateListingThread + ".jsonl"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(rewrite(text)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runCodexCounted runs the recorded stream through Run, which keeps the tool counts.
func runCodexCounted(t *testing.T, home string, gated bool) (Result, []Event) {
	t.Helper()
	stream, err := filepath.Abs(filepath.Join("testdata", "codex-gate-exec.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", home)
	b := NewCodexBackend()
	b.name, b.candidates = os.Args[0], nil
	task := Task{Prompt: "go", Dir: t.TempDir()}
	if gated {
		installFakeGate(t)
		t.Setenv("HRN_CODEX_GATE_STREAM", stream)
		t.Setenv("HRN_CODEX_GATE_LOG", filepath.Join(t.TempDir(), "started.json"))
		real := b.args
		b.args = func(task Task, prompt string) []string {
			return append([]string{"-test.run=^TestHelperCLICodexGate$", "--"}, real(task, prompt)...)
		}
		task.Options = Options{Mode: ModeYolo, Gate: true}
	} else {
		t.Setenv("HRN_CODEX_RECORDED_STREAM", stream)
		b.args = func(Task, string) []string { return []string{"-test.run=^TestHelperCodexRecordedStream$", "--"} }
	}
	var events []Event
	res := Run(context.Background(), b, task, func(e Event) { events = append(events, e) })
	if res.Status != StatusOK || res.Session != gateListingThread {
		t.Fatalf("result = %s %q session %q", res.Status, res.Error, res.Session)
	}
	return res, events
}

func toolCallTexts(events []Event, tool string) []string {
	var got []string
	for _, e := range events {
		if e.Kind == EventToolCall && e.Tool == tool {
			got = append(got, e.Text)
		}
	}
	return got
}

var recordedListings = []string{
	"const t = ALL_TOOLS.filter(x=>/run_command/.test(x.name)); text(t);\n",
	"text(ALL_TOOLS.filter(x=>/read_original/.test(x.name)));\n",
}

func TestCodexCountsExecScriptsThatOnlyListTools(t *testing.T) {
	for _, tc := range []struct {
		name    string
		gated   bool
		command string
		rewrite func(string) string
	}{
		{name: "gated", gated: true, command: "shell", rewrite: same},
		{name: "ungated", command: "mcp__hrn_gate__run_command", rewrite: same},
		{name: "rollout also holds an earlier run's listing", gated: true, command: "shell", rewrite: func(s string) string {
			old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
			first, rest, _ := strings.Cut(s, "\n")
			return first + "\n" +
				`{"timestamp":"` + old + `","type":"response_item","payload":{"type":"custom_tool_call","call_id":"old","name":"exec","input":"text(ALL_TOOLS)"}}` + "\n" +
				`{"timestamp":"` + old + `","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"old","output":"[]"}}` + "\n" + rest
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			placeGateRollout(t, home, time.Now().Add(time.Minute), tc.rewrite)
			res, events := runCodexCounted(t, home, tc.gated)
			if got := toolCallTexts(events, "exec"); !slices.Equal(got, recordedListings) {
				t.Fatalf("exec calls = %q, want %q", got, recordedListings)
			}
			if res.ToolCalls != 4 || len(res.ToolStats) != 2 || res.ToolStats[tc.command].Calls != 2 {
				t.Fatalf("tool_calls = %d, tool_stats = %+v", res.ToolCalls, res.ToolStats)
			}
			// Each listing's output starts with codex's script header and names the tool it found.
			if exec := res.ToolStats["exec"]; exec.Calls != 2 || exec.Errors != 0 || exec.ResultBytes < 2*len("Script completed\n") {
				t.Fatalf("exec stats = %+v", exec)
			}
			var listed int
			for _, e := range events {
				if e.Kind == EventToolResult && e.Tool == "exec" && strings.Contains(e.Text, "mcp__hrn_gate__") {
					listed++
				}
			}
			if listed != 2 {
				t.Fatalf("exec results naming the gate's tools = %d, want 2", listed)
			}
		})
	}
}

func TestGatedCodexAddsNoExecCallsItCannotTieToThisRun(t *testing.T) {
	for _, tc := range []struct {
		name  string
		place func(t *testing.T, home string)
	}{
		{name: "no rollout", place: func(*testing.T, string) {}},
		{name: "rollout lines from before the run", place: func(t *testing.T, home string) {
			placeGateRollout(t, home, time.Now().Add(-time.Hour), same)
		}},
		{name: "rollout shows fewer tool items than the stream", place: func(t *testing.T, home string) {
			placeGateRollout(t, home, time.Now().Add(time.Minute), func(s string) string {
				var kept []string
				for line := range strings.SplitSeq(s, "\n") {
					if !strings.Contains(line, `"exec-f7c01054-f986-49a5-999c-571c27f3c262"`) {
						kept = append(kept, line)
					}
				}
				return strings.Join(kept, "\n")
			})
		}},
		{name: "rollout of another session", place: func(t *testing.T, home string) {
			placeGateRollout(t, home, time.Now().Add(time.Minute), func(s string) string {
				return strings.Replace(s, `"id":"`+gateListingThread+`"`, `"id":"another-thread"`, 1)
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			tc.place(t, home)
			res, events := runCodexCounted(t, home, true)
			if got := toolCallTexts(events, "exec"); got != nil {
				t.Fatalf("exec calls = %q, want none", got)
			}
			if res.ToolCalls != 2 || len(res.ToolStats) != 1 || res.ToolStats["shell"].Calls != 2 {
				t.Fatalf("tool_calls = %d, tool_stats = %+v", res.ToolCalls, res.ToolStats)
			}
		})
	}
}

var rolloutBase = time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)

// rolloutLine stamps a line at a second past rolloutBase; an item starts at its startMS past it.
func rolloutLine(sec float64, kind, payload string) string {
	at := rolloutBase.Add(time.Duration(sec * float64(time.Second)))
	return fmt.Sprintf(`{"timestamp":%q,"type":%q,"payload":%s}`, at.Format(time.RFC3339Nano), kind, payload)
}

func rolloutItem(sec float64, itemType string, startSec float64) string {
	start := rolloutBase.Add(time.Duration(startSec * float64(time.Second))).UnixMilli()
	return rolloutLine(sec, "event_msg", fmt.Sprintf(`{"type":"item_completed","item":{"type":%q},"started_at_ms":%d}`, itemType, start))
}

func readRolloutLines(t *testing.T, lines []string) codexRolloutRead {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	all := append([]string{rolloutLine(0, "session_meta", `{"id":"s1","cli_version":"0.167.0"}`)}, lines...)
	if err := os.WriteFile(path, []byte(strings.Join(all, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, ok := readCodexRollout(path, "s1", rolloutBase.Add(time.Second/2))
	if !ok {
		t.Fatal("rollout not read")
	}
	return r
}

func callSummaries(events []Event) []string {
	var got []string
	for _, e := range events {
		got = append(got, fmt.Sprintf("%s %s %q %v", e.Kind, e.Tool, e.Text, e.IsError))
	}
	return got
}

func TestCodexRolloutCountsOnlyCallsThatRanNoCountedTool(t *testing.T) {
	r := readRolloutLines(t, []string{
		// An earlier run's lines are not this run's.
		rolloutLine(-100, "response_item", `{"type":"custom_tool_call","call_id":"c0","name":"exec","input":"text(ALL_TOOLS)"}`),
		rolloutItem(-99, "CommandExecution", -99),
		rolloutLine(-98, "response_item", `{"type":"custom_tool_call_output","call_id":"c0","output":"[]"}`),
		// A script that edits a file: its FileChange item is in the stream already.
		rolloutLine(1, "response_item", `{"type":"custom_tool_call","call_id":"c1","name":"exec","input":"await tools.apply_patch(p)"}`),
		rolloutItem(3, "FileChange", 2),
		rolloutLine(4, "response_item", `{"type":"custom_tool_call_output","call_id":"c1","output":"{}"}`),
		// A script that only views an image, which the stream does not report.
		rolloutLine(5, "response_item", `{"type":"custom_tool_call","call_id":"c2","name":"exec","input":"await tools.view_image(p)"}`),
		rolloutItem(6, "ImageView", 5.5),
		rolloutLine(7, "response_item", `{"type":"custom_tool_call_output","call_id":"c2","output":[{"type":"input_text","text":"seen"}]}`),
		// Two scripts at once while a command runs: either may own it, so neither is added.
		rolloutLine(8, "response_item", `{"type":"custom_tool_call","call_id":"c3","name":"exec","input":"text(1)"}`),
		rolloutLine(8, "response_item", `{"type":"custom_tool_call","call_id":"c4","name":"exec","input":"await tools.exec_command(c)"}`),
		rolloutItem(10, "CommandExecution", 9),
		rolloutLine(11, "response_item", `{"type":"custom_tool_call_output","call_id":"c3","output":"1"}`),
		rolloutLine(11, "response_item", `{"type":"custom_tool_call_output","call_id":"c4","output":"ok"}`),
		// A direct tool call with a plain-text output.
		rolloutLine(12, "response_item", `{"type":"function_call","call_id":"c5","name":"update_plan","arguments":"{\"plan\":[]}"}`),
		rolloutLine(13, "response_item", `{"type":"function_call_output","call_id":"c5","output":"Plan updated"}`),
		// A script returns while its command runs on; the command completes during a later wait.
		rolloutLine(14, "response_item", `{"type":"custom_tool_call","call_id":"c6","name":"exec","input":"await tools.exec_command(long)"}`),
		rolloutLine(15, "response_item", `{"type":"custom_tool_call_output","call_id":"c6","output":"Script running with cell ID 1"}`),
		rolloutLine(16, "response_item", `{"type":"function_call","call_id":"c7","name":"wait","arguments":"{\"cell_id\":\"1\"}"}`),
		rolloutItem(20, "CommandExecution", 14.5),
		rolloutLine(21, "response_item", `{"type":"function_call_output","call_id":"c7","output":"Script completed"}`),
		// A script that throws, then a second call reusing its id, which is ignored.
		rolloutLine(22, "response_item", `{"type":"custom_tool_call","call_id":"c8","name":"exec","input":"throw 1"}`),
		rolloutLine(23, "response_item", `{"type":"custom_tool_call_output","call_id":"c8","output":[{"type":"input_text","text":"Script failed\nError: 1"}]}`),
		rolloutLine(24, "response_item", `{"type":"custom_tool_call","call_id":"c8","name":"exec","input":"again"}`),
		// A script cut off by the run's end has no output.
		rolloutLine(25, "response_item", `{"type":"custom_tool_call","call_id":"c9","name":"exec","input":"text(ALL_TOOLS)"}`),
	})
	if r.counted != 3 {
		t.Fatalf("counted = %d, want 3", r.counted)
	}
	want := []string{
		fmt.Sprintf("%s exec %q false", EventToolCall, "await tools.view_image(p)"),
		fmt.Sprintf("%s exec %q false", EventToolResult, "seen"),
		fmt.Sprintf("%s update_plan %q false", EventToolCall, `{"plan":[]}`),
		fmt.Sprintf("%s update_plan %q false", EventToolResult, "Plan updated"),
		fmt.Sprintf("%s wait %q false", EventToolCall, `{"cell_id":"1"}`),
		fmt.Sprintf("%s wait %q false", EventToolResult, "Script completed"),
		fmt.Sprintf("%s exec %q false", EventToolCall, "throw 1"),
		fmt.Sprintf("%s exec %q true", EventToolResult, "Script failed\nError: 1"),
		fmt.Sprintf("%s exec %q false", EventToolCall, "text(ALL_TOOLS)"),
	}
	if got := callSummaries(r.calls); !slices.Equal(got, want) {
		t.Fatalf("calls:\n%q\nwant:\n%q", got, want)
	}
}

func TestCodexRolloutAddsNothingWhenAnItemCannotBePlaced(t *testing.T) {
	listing := []string{
		rolloutLine(1, "response_item", `{"type":"custom_tool_call","call_id":"c1","name":"exec","input":"text(ALL_TOOLS)"}`),
		rolloutLine(2, "response_item", `{"type":"custom_tool_call_output","call_id":"c1","output":"[]"}`),
	}
	for _, tc := range []struct {
		name string
		item string
	}{
		{name: "no start time", item: rolloutLine(4, "event_msg", `{"type":"item_completed","item":{"type":"CommandExecution"}}`)},
		{name: "started outside every call", item: rolloutItem(4, "CommandExecution", 3)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := readRolloutLines(t, append(slices.Clone(listing), tc.item))
			if r.counted != 1 || r.calls != nil {
				t.Fatalf("counted = %d, calls = %q", r.counted, callSummaries(r.calls))
			}
		})
	}
}

func TestCodexStreamCountsFileEditsAndMCPTools(t *testing.T) {
	lines := []string{
		`{"type":"item.started","item":{"id":"item_1","type":"file_change","changes":[{"path":"a.txt","kind":"add"},{"path":"b.txt","kind":"update"}],"status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"item_1","type":"file_change","changes":[{"path":"a.txt","kind":"add"},{"path":"b.txt","kind":"update"}],"status":"completed"}}`,
		`{"type":"item.started","item":{"id":"item_2","type":"file_change","changes":[{"path":"c.txt","kind":"delete"}],"status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"item_2","type":"file_change","changes":[{"path":"c.txt","kind":"delete"}],"status":"failed"}}`,
		`{"type":"item.started","item":{"id":"item_3","type":"mcp_tool_call","server":"docs","tool":"search","arguments":{"query":"x"},"status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"item_3","type":"mcp_tool_call","server":"docs","tool":"search","arguments":{"query":"x"},"result":{"content":[{"type":"text","text":"found"}]},"status":"completed"}}`,
		// A field of an unexpected shape loses only that field.
		`{"type":"item.started","item":{"id":"item_4","type":"file_change","changes":"d.txt","status":"in_progress"}}`,
		`{"type":"item.started","item":{"id":"item_5","type":"mcp_tool_call","server":"hrn_gate","tool":"run_command","arguments":{"command":"ls"},"status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"item_5","type":"mcp_tool_call","server":"hrn_gate","tool":"run_command","arguments":{"command":"ls"},"result":"odd","status":"completed"}}`,
	}
	want := []string{
		fmt.Sprintf("%s apply_patch %q false", EventToolCall, "add a.txt\nupdate b.txt"),
		fmt.Sprintf("%s apply_patch %q false", EventToolResult, ""),
		fmt.Sprintf("%s apply_patch %q false", EventToolCall, "delete c.txt"),
		fmt.Sprintf("%s apply_patch %q true", EventToolResult, ""),
		fmt.Sprintf("%s mcp__docs__search %q false", EventToolCall, `{"query":"x"}`),
		fmt.Sprintf("%s mcp__docs__search %q false", EventToolResult, "found"),
		fmt.Sprintf("%s apply_patch %q false", EventToolCall, ""),
	}
	for _, tc := range []struct {
		name  string
		parse func(line []byte, emit func(Event), res *Result) bool
		gate  string
	}{
		// Without the gate, a server of the gate's name is just another MCP server.
		{name: "ungated", parse: parseCodexStream, gate: "mcp__hrn_gate__run_command"},
		{name: "gated", parse: newCodexGateParse("", ""), gate: "shell"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []Event
			var res Result
			for _, line := range lines {
				if !tc.parse([]byte(line), func(e Event) { got = append(got, e) }, &res) {
					t.Fatalf("not parsed: %s", line)
				}
			}
			if res.Error != "" {
				t.Fatalf("error = %q", res.Error)
			}
			w := append(slices.Clone(want),
				fmt.Sprintf("%s %s %q false", EventToolCall, tc.gate, "ls"),
				fmt.Sprintf("%s %s %q false", EventToolResult, tc.gate, ""))
			if got := callSummaries(got); !slices.Equal(got, w) {
				t.Fatalf("events:\n%q\nwant:\n%q", got, w)
			}
		})
	}
}
