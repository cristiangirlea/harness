package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

func contextTokens(t *testing.T, res Result) int64 {
	t.Helper()
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	var n int64
	if raw, ok := fields["context_tokens"]; ok {
		if err := json.Unmarshal(raw, &n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Error("unknown context must be omitted")
		}
	}
	return n
}

func TestResultKeepsReportedContextAndOmitsUnknownContext(t *testing.T) {
	for _, n := range []int64{285000, 0} {
		var res Result
		data := `{"status":"ok","turns":1}`
		if n != 0 {
			data = fmt.Sprintf(`{"status":"ok","turns":1,"context_tokens":%d}`, n)
		}
		if err := json.Unmarshal([]byte(data), &res); err != nil {
			t.Fatal(err)
		}
		if got := contextTokens(t, res); got != n {
			t.Errorf("context = %d, want %d", got, n)
		}
	}
}

func TestAPIRecordsOnlyTheLastMessagesInputIncludingBothCaches(t *testing.T) {
	first := toolTurn("list", "list_files", `{"pattern":"*"}`)
	last := textTurn("done")
	last[0] = `{"type":"message_start","message":{"id":"last","role":"assistant","model":"claude-opus-5","usage":{"input_tokens":5000,"cache_read_input_tokens":270000,"cache_creation_input_tokens":10000}}}`
	res, _, _ := runFake(t, [][]string{first, last}, Task{Prompt: "list", Dir: t.TempDir()})
	if res.Status != StatusOK || res.Turns != 2 {
		t.Fatalf("result = %+v", res)
	}
	if got := contextTokens(t, res); got != 285000 {
		t.Errorf("context = %d, want last input plus caches = 285000", got)
	}
	if res.InputTokens != 5100 || res.CacheRead != 270000 || res.CacheWrite != 10000 {
		t.Errorf("run totals changed: %+v", res)
	}
}

func TestCLIsRecordLastModelInputWithoutGuessingFromTotals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		backend *CLIBackend
		want    int64
	}{
		{"claude", NewClaudeBackend(), 285000},
		{"claude-subagent", NewClaudeBackend(), 285000},
		{"opencode", NewOpencodeBackend(), 285000},
		{"codex", NewCodexBackend(), 0},
		{"gemini", NewGeminiBackend(), 0},
		{"claude-totals", NewClaudeBackend(), 0},
		{"claude-missing-last", NewClaudeBackend(), 0},
		{"opencode-missing-last", NewOpencodeBackend(), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HRN_CONTEXT_HELPER", tc.name)
			b := helperCLI(tc.backend.parse)
			b.finalize = tc.backend.finalize
			b.args = func(Task, string) []string { return []string{"-test.run=^TestHelperCLIContext$", "--"} }
			res := b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir()}, func(Event) {})
			if res.Status != StatusOK {
				t.Fatalf("result = %+v", res)
			}
			if got := contextTokens(t, res); got != tc.want {
				t.Errorf("context = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestHelperCLIContext(t *testing.T) {
	switch os.Getenv("HRN_CONTEXT_HELPER") {
	case "claude", "claude-subagent", "claude-missing-last":
		fmt.Println(`{"type":"assistant","message":{"content":[],"usage":{"input_tokens":6000,"cache_read_input_tokens":300000,"cache_creation_input_tokens":20000}}}`)
		fmt.Println(`{"type":"assistant","parent_tool_use_id":null,"message":{"content":[],"usage":{"input_tokens":5000,"cache_read_input_tokens":270000,"cache_creation_input_tokens":10000}}}`)
		fmt.Println(`{"type":"user","message":{"content":[]}}`)
		if os.Getenv("HRN_CONTEXT_HELPER") == "claude-subagent" {
			fmt.Println(`{"type":"assistant","parent_tool_use_id":"toolu_x","message":{"content":[],"usage":{"input_tokens":1000,"cache_read_input_tokens":20000,"cache_creation_input_tokens":3000}}}`)
		}
		if os.Getenv("HRN_CONTEXT_HELPER") == "claude-missing-last" {
			fmt.Println(`{"type":"assistant","message":{"content":[]}}`)
		}
		fallthrough
	case "claude-totals":
		fmt.Println(`{"type":"result","subtype":"success","num_turns":2,"usage":{"input_tokens":11000,"cache_read_input_tokens":570000,"cache_creation_input_tokens":30000}}`)
	case "opencode", "opencode-missing-last":
		fmt.Println(`{"type":"step_finish","part":{"tokens":{"input":6000,"output":900,"cache":{"read":300000,"write":20000}}}}`)
		fmt.Println(`{"type":"step_finish","part":{"tokens":{"input":5000,"output":700,"cache":{"read":270000,"write":10000}}}}`)
		if os.Getenv("HRN_CONTEXT_HELPER") == "opencode-missing-last" {
			fmt.Println(`{"type":"step_finish","part":{}}`)
		}
	case "codex":
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":611000,"cached_input_tokens":570000,"output_tokens":1600}}`)
	case "gemini":
		fmt.Println(`{"response":"done","stats":{"models":{"gemini":{"tokens":{"prompt":611000,"cached":570000,"candidates":1600}}}}}`)
	default:
		return
	}
	os.Exit(0)
}
