package agent

import (
	"slices"
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
	args := strings.Join(b.args(Task{Options: Options{Mode: ModeAuto, Model: "sonnet", MaxTurns: 5}}, ""), " ")
	for _, want := range []string{"-p", "stream-json", "--permission-mode acceptEdits", "--model sonnet", "--max-turns 5"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q missing %q", args, want)
		}
	}
}

// Claude Code's deny rules hold in every permission mode, bypassPermissions
// included, so the guard hands them over in every mode: publishing commands
// in both its shells, and the secret files. --allow-dangerous drops them.
func TestClaudeCarriesTheGuardsDenyRules(t *testing.T) {
	b := NewClaudeBackend()
	for _, mode := range []Mode{ModeYolo, ModeAuto, ModeReadOnly} {
		args := b.args(Task{Options: Options{Mode: mode}}, "")
		i := slices.Index(args, "--disallowedTools")
		if i < 0 {
			t.Fatalf("mode %s: no --disallowedTools in %q", mode, args)
		}
		rules := args[i+1:]
		if j := slices.IndexFunc(rules, func(s string) bool { return strings.HasPrefix(s, "-") }); j >= 0 {
			rules = rules[:j]
		}
		for _, want := range []string{"Bash(git push *)", "PowerShell(git push *)", "Bash(gh pr merge *)", "Bash(gh release create *)",
			"Bash(npx wrangler deploy *)", "Bash(npm publish *)", "Bash(npm run deploy *)", "Bash(terraform apply *)", "Bash(helm upgrade *)",
			"Read(//**/.env)", "Read(//**/*.pem)", "Read(~/.ssh/**)", "Read(//**/.private/**)", "Read(~/.kube/**)", "Read(//**/.wrangler/**)"} {
			if !slices.Contains(rules, want) {
				t.Errorf("mode %s: deny rules %q lack %q", mode, rules, want)
			}
		}
	}
	if args := b.args(Task{Options: Options{Mode: ModeYolo, AllowDangerous: true}}, ""); slices.Contains(args, "--disallowedTools") {
		t.Errorf("--allow-dangerous still hands over deny rules: %q", args)
	}
}

func TestVersionLess(t *testing.T) {
	if !versionLess("2.1.9", "2.1.270") || versionLess("2.1.271", "2.1.270") {
		t.Error("versionLess compares numerically")
	}
}

func TestParseClaudeStreamErrorText(t *testing.T) {
	var res Result
	line := `{"type":"result","subtype":"success","is_error":true,"num_turns":1,"result":"Not logged in · Please run /login"}`
	if !parseClaudeStream([]byte(line), func(Event) {}, &res) {
		t.Fatal("result line not parsed")
	}
	if res.Status != StatusError || res.Error != "Not logged in · Please run /login" {
		t.Fatalf("a failure reported with subtype success must carry the result text: %+v", res)
	}
	res = Result{}
	line = `{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":5,"result":"partial answer"}`
	parseClaudeStream([]byte(line), func(Event) {}, &res)
	if res.Status != StatusMaxTurns || res.Error != "error_max_turns" || res.Final != "partial answer" {
		t.Fatalf("a specific subtype stays the error: %+v", res)
	}
}

func TestParseClaudeStreamErrorSubtypes(t *testing.T) {
	for _, tc := range []struct{ line, wantErr string }{
		{`{"type":"result","subtype":"success","is_error":true,"num_turns":1,"result":""}`, "unknown error"},
		{`{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":2,"result":"Tool Bash was denied"}`, "error_during_execution: Tool Bash was denied"},
		{`{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":2,"result":""}`, "error_during_execution"},
	} {
		var res Result
		parseClaudeStream([]byte(tc.line), func(Event) {}, &res)
		if res.Status != StatusError || res.Error != tc.wantErr {
			t.Errorf("%s: got status=%s error=%q, want %q", tc.line, res.Status, res.Error, tc.wantErr)
		}
	}
}

// Claude Code reports a call its deny rules refused as a system line whose
// message is a string, unlike the messages hrn decodes. It must not come out
// as raw JSON: the tool result after it already says the call was refused.
func TestAStreamLineOfAnUnmodelledShapeIsNotPrintedRaw(t *testing.T) {
	line := `{"type":"system","subtype":"permission_denied","tool_name":"Bash","message":"Permission to use Bash with command git push origin main has been denied.","session_id":"c1d19a4a"}`
	var events []Event
	var res Result
	if !parseClaudeStream([]byte(line), func(e Event) { events = append(events, e) }, &res) {
		t.Fatal("a claude stream line was handed back as plain text")
	}
	if len(events) != 0 {
		t.Errorf("events = %+v; the refusal is reported by the tool result that follows", events)
	}
	if res.Session != "c1d19a4a" {
		t.Errorf("session = %q; every line carries it", res.Session)
	}
}

// The deny rules are a list of values after one flag: a flag after them ends
// the list, so MCP configs and a resumed session still reach the CLI as such.
func TestTheDenyRulesEndBeforeTheNextFlag(t *testing.T) {
	args := NewClaudeBackend().args(Task{Options: Options{Mode: ModeYolo, MCP: []string{"servers.json"}}, ForkSession: "0b1c2d3e-0000-4000-8000-000000000000"}, "")
	i := slices.Index(args, "--disallowedTools")
	for _, flag := range []string{"--mcp-config", "--resume", "--fork-session"} {
		if j := slices.Index(args, flag); j < i {
			t.Errorf("%s comes before the deny rules or is missing: %q", flag, args)
		}
	}
	if shown := strings.Join(shownArgs(args), " "); strings.Contains(shown, "Bash(") || !strings.Contains(shown, "guard rules> --mcp-config servers.json") {
		t.Errorf("the run's info line should count the rules and keep the flags after them: %s", shown)
	}
}

// A line with one field of an unexpected type is still read for the rest: a
// result whose turn count came as a string keeps its answer and its status.
func TestAMistypedFieldDoesNotLoseTheRestOfTheLine(t *testing.T) {
	line := `{"type":"result","subtype":"success","is_error":false,"num_turns":"3","result":"the answer","session_id":"s1"}`
	var res Result
	if !parseClaudeStream([]byte(line), func(Event) {}, &res) {
		t.Fatal("the line was not taken as a stream line")
	}
	if res.Final != "the answer" || res.Status != StatusOK || res.Session != "s1" {
		t.Fatalf("result = %+v; want the answer, ok and the session despite the mistyped num_turns", res)
	}
}
