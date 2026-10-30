// name: tool-shapes-tests
// summary: Check tool shape classification, normalisation and record accounting.
// effects: read, exec
// parameters: none
// example: go test -count=1 ./scripts/tool-shapes
// hash: 1fc1a1521c20289aa2cd301807fa74d54b7c0e4cb242061a24f2dbca11001402
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
)

func TestClassifyNamesTheShapeOfEachToolResult(t *testing.T) {
	for _, tt := range []struct{ text, tool, want string }{
		{"go test ./...\nok pkg 1s", "shell", "test"}, {"FAIL pkg", "", "test"}, {"--- PASS: TestX", "", "test"},
		{"--- a/main.go\n+++ b/main.go", "shell", "terminal"},
		{"=== RUN TestX", "shell", "test"}, {"--- FAIL: TestX", "shell", "test"},
		{"--- SKIP: TestX", "shell", "terminal"}, {"--- BENCH: BenchmarkX", "shell", "terminal"},
		{"2026-10-14T12:00:00Z ready", "", "log"}, {"[INFO] ready", "", "log"}, {"ERROR: failed", "", "log"},
		{`{"a":1}`, "shell", "json"}, {`[1,2]`, "", "json"}, {"a,b\n1,2", "", "terminal"}, {"a\tb\n1\t2", "", "terminal"},
		{"a,b\n1,2,3", "shell", "terminal"}, {"hello", "Bash", "terminal"}, {"hello", "exec_command", "terminal"},
		{"hello", "read_file", "terminal"}, {"", "", "other"},
		{"{\"Action\":\"run\",\"Test\":\"TestX\"}\n{\"Action\":\"pass\",\"Test\":\"TestX\"}\n", "shell", "terminal"},
		{"{\"a\":1}\n{\"b\":2}", "shell", "terminal"},
	} {
		t.Run(tt.want+tt.text, func(t *testing.T) {
			if got := classify(tt.text, tt.tool); got != tt.want {
				t.Fatalf("got %s want %s", got, tt.want)
			}
		})
	}
}

func TestClassificationMatchesShaping(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{`{"message":"FAIL thing"}`, "json"}, {`42`, "terminal"},
		{"a,b\n1,2", "terminal"}, {"a,b\n" + strings.Repeat("1,\"a,b\"\n2,c\n", 3), "terminal"},
		{"a,b\n" + strings.Repeat("1,2\n", 5), "table"},
	} {
		if got := classify(tc.text, "shell"); got != tc.want {
			t.Errorf("%q: got %s want %s", tc.text, got, tc.want)
		}
	}
}

func TestInlineScriptsAreDetectedAndNormalised(t *testing.T) {
	for _, tt := range []struct{ cmd, want string }{
		{"cat <<'EOF'\nx\nEOF", "heredoc"}, {`python -c "print(1)"`, "python"}, {`python3 -c 'print(2)'`, "python"},
		{`node -e 'x()'`, "node"}, {`perl -e 'print 1'`, "perl"}, {"go run /tmp/a.go", "go"},
		{`go run "C:\Users\x\AppData\Local\Temp\a.go"`, "go"}, {"go run ./scripts/answers.go", ""}, {"echo python -c", ""},
	} {
		if got := inlineShape(tt.cmd); got != tt.want {
			t.Errorf("%q got %q want %q", tt.cmd, got, tt.want)
		}
	}
	a := normalize(`python -c "print(123)" /tmp/abc.py longidentifierone 100`)
	b := normalize("python   -c 'print(456)' C:\\Temp\\def.py longidentifiertwo 200")
	if a != b {
		t.Fatalf("normal forms differ: %q %q", a, b)
	}
	if normalize("node -e x") == normalize("perl -e x") {
		t.Fatal("distinct forms merged")
	}
	if inlineShape("  python -c 'print(1)'") != "python" {
		t.Error("leading whitespace")
	}
	if normalize("C:/Temp/a.go 123 identifierabc") != normalize("/tmp/b.go 456 identifierxyz") {
		t.Error("forward slash paths")
	}
	if normalize("abcdefghijkl") != "abcdefghijkl" || normalize("abcdefghijklm") != "<identifier>" {
		t.Fatal("identifier boundary")
	}
}

func writeRun(t *testing.T, home, id, backend, status, events string) {
	t.Helper()
	dir := filepath.Join(home, "runs", id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"task.json": `{"backend":"` + backend + `","prompt":"private"}`, "result.json": `{"status":"` + status + `"}`, "events.jsonl": events} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMeasureCountsFinishedRunsInTheWindowWithoutQuotingThem(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.Local)
	events := `{"t":"2026-10-14T10:00:00Z","kind":"tool_call","tool":"Bash","input":{"command":"cat <<EOF\nsecret\nEOF"}}
{"kind":"tool_result","tool":"Bash","text":"ok pkg"}
{"kind":"usage","usage":{"input_tokens":10,"cache_read_tokens":90,"cache_write_tokens":999}}
`
	writeRun(t, home, "20261014-100000-api", "api", "ok", events)
	writeRun(t, home, "20261014-100001-claude", "claude", "error", events)
	writeRun(t, home, "20261014-100002-no-usage", "codex", "ok", `{"kind":"tool_result","text":"{}"}`)
	writeRun(t, home, "20260901-100000-old", "api", "ok", events)
	writeRun(t, home, "20261015-100000-future", "api", "ok", events)
	writeRun(t, home, "20261014-100003-bad", "api", "ok", events+"broken")
	writeRun(t, home, "20261014-100004-unfinished", "api", "", events)
	r, err := measure(home, 30, now)
	if err != nil {
		t.Fatal(err)
	}
	if r.Runs.Read != 3 || r.Runs.WithUsage != 2 || r.Runs.Unreadable != 1 {
		t.Fatalf("runs %+v", r.Runs)
	}
	if r.InputTokens != 200 || r.ToolResultBytes != 14 || r.Shapes[0].Bytes != 12 || *r.Shapes[0].InputShare != .015 {
		t.Fatalf("totals %+v", r)
	}
	if len(r.InlineScripts) != 1 || r.InlineScripts[0].Count != 2 || r.InlineScripts[0].Runs != 2 || r.Claude.ToolResultBytes != 6 {
		t.Fatalf("groups %+v", r)
	}
	data, _ := json.Marshal(r)
	if strings.Contains(string(data), "secret") || strings.Contains(string(data), "private") || strings.Contains(string(data), home) {
		t.Fatal("record text leaked")
	}
	if _, err := measure(filepath.Join(home, "missing"), 30, now); err == nil {
		t.Fatal("missing home accepted")
	}
}

func TestReplayedShares(t *testing.T) {
	command := "python -c 'x()'"
	call := `{"kind":"tool_call","tool":"shell","text":"` + command + `"}`
	result := `{"kind":"tool_result","tool":"shell","text":"abcdefgh"}`
	usage := `{"kind":"usage","usage":{"input_tokens":100}}`
	for _, tt := range []struct {
		name, totals, events        string
		resultWeight, commandWeight float64
	}{
		{"usage order", `{"status":"ok","turns":99,"input_tokens":999}`, strings.Join([]string{usage, call, usage, result, usage, call, result, usage, result}, "\n"), 3, 4},
		{"usage without figures", `{"status":"ok","turns":99,"input_tokens":300}`, strings.Join([]string{call, result, `{"kind":"usage"}`, call, result, `{"kind":"usage"}`}, "\n"), 3, 3},
		{"flat turns", `{"status":"ok","turns":4,"input_tokens":300}`, strings.Join([]string{call, result, call, result, result}, "\n"), 6, 5},
		{"wrapped turns", `{"result":{"status":"ok","turns":4,"input_tokens":300}}`, strings.Join([]string{call, result, call, result, result}, "\n"), 6, 5},
		{"clamp", `{"status":"ok","turns":1,"input_tokens":300}`, strings.Join([]string{call, result, call, result, result}, "\n"), 0, 0},
		{"missing turns", `{"status":"ok","input_tokens":300}`, strings.Join([]string{call, result, call, result, result}, "\n"), 0, 0},
		{"no tokens", `{"status":"ok","turns":4}`, strings.Join([]string{call, result, call, result, result}, "\n"), 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			id := "20261014-100000-replay"
			writeRun(t, home, id, "claude", "ok", tt.events)
			if err := os.WriteFile(filepath.Join(home, "runs", id, "result.json"), []byte(tt.totals), 0600); err != nil {
				t.Fatal(err)
			}
			r, err := measure(home, 30, time.Date(2026, 10, 14, 12, 0, 0, 0, time.Local))
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Shapes []struct {
					Replayed *float64 `json:"input_token_share_replayed_estimate"`
				} `json:"shapes"`
				Inline   *float64 `json:"inline_script_input_share_replayed_estimate"`
				Repeated *float64 `json:"repeated_inline_script_input_share_replayed_estimate"`
			}
			check := func(data []byte) {
				t.Helper()
				if err := json.Unmarshal(data, &got); err != nil {
					t.Fatal(err)
				}
				for key, want := range map[string]float64{
					"input_token_share_replayed_estimate":                  2 * tt.resultWeight / float64(max(1, r.InputTokens)),
					"inline_script_input_share_replayed_estimate":          float64(len(command)) / 4 * tt.commandWeight / float64(max(1, r.InputTokens)),
					"repeated_inline_script_input_share_replayed_estimate": float64(len(command)) / 4 * tt.commandWeight / float64(max(1, r.InputTokens)),
				} {
					value := got.Inline
					if key == "input_token_share_replayed_estimate" {
						value = got.Shapes[4].Replayed
					}
					if key == "repeated_inline_script_input_share_replayed_estimate" {
						value = got.Repeated
					}
					if !strings.Contains(string(data), `"`+key+`"`) {
						t.Errorf("missing %s", key)
						continue
					}
					if tt.name == "no tokens" {
						if value != nil {
							t.Errorf("%s without denominator", key)
						}
						continue
					}
					if value == nil || *value != want {
						t.Errorf("%s = %v want %g", key, value, want)
					}
				}
			}
			check(data)
			data, _ = json.Marshal(r.Claude)
			check(data)
		})
	}
}

func TestInlineScriptTotalsCountEveryLabelBeyondTheTopGroups(t *testing.T) {
	s := newStats()
	var events []event
	for i := range 25 {
		events = append(events, event{Kind: "tool_call", Tool: "shell", Text: fmt.Sprintf("cat <<EOF\n%c\nEOF", 'a'+i)})
	}
	events = append(events, event{Kind: "tool_call", Tool: "shell", Text: "python -c 'x()'"})
	s.add(events, nil, 0)
	s.add([]event{{Kind: "tool_call", Tool: "shell", Text: "python3 -c 'y()'"}}, nil, 0)
	s.finish()
	want := map[string]group{
		"heredoc": {Count: 25, Runs: 1, Bytes: 25 * 15},
		"python":  {Count: 2, Runs: 2, Bytes: 31},
		"node":    {}, "perl": {}, "go": {},
	}
	if len(s.InlineTotals) != len(want) {
		t.Fatalf("totals %+v", s.InlineTotals)
	}
	for _, g := range s.InlineTotals {
		w := want[g.Shape]
		if g.Count != w.Count || g.Runs != w.Runs || g.Bytes != w.Bytes {
			t.Errorf("%s: %+v want %+v", g.Shape, g, w)
		}
	}
	for _, g := range s.InlineScripts {
		if g.Shape == "python" {
			t.Fatal("python groups expected below the top 20")
		}
	}
}

func TestNoUsageAndCap(t *testing.T) {
	s := newStats()
	for i := 0; i < 25; i++ {
		s.groups[strings.Repeat("x", i+1)] = &group{Shape: "node", Count: 1, Runs: 1}
	}
	s.finish()
	if len(s.InlineScripts) != 20 || s.Shapes[0].InputShare != nil {
		t.Fatal("cap or missing usage")
	}
}

func TestTokenCoverage(t *testing.T) {
	for _, tt := range []struct {
		name, backend, result, usage string
		want                         int64
	}{
		{"flat api", "api", `{"status":"ok","input_tokens":100,"cache_read_tokens":50}`, "", 150},
		{"cache only api", "api", `{"status":"ok","cache_read_tokens":50}`, "", 50},
		{"wrapped claude", "claude", `{"result":{"status":"ok","input_tokens":100,"cache_read_tokens":50}}`, "", 150},
		{"old codex", "codex", `{"status":"ok","input_tokens":100,"cache_read_tokens":50}`, "", 100},
		{"wrapped codex", "codex", `{"result":{"status":"ok","backend":"codex","input_tokens":100,"cache_read_tokens":50,"usage_metadata":{"input_semantics":"inclusive_cache"}}}`, "", 100},
		{"usage wins", "api", `{"status":"ok","input_tokens":900,"cache_read_tokens":900}`, "\n{\"kind\":\"usage\",\"usage\":{\"input_tokens\":20,\"cache_read_tokens\":30}}\n{\"kind\":\"usage\",\"usage\":{\"input_tokens\":40,\"cache_read_tokens\":60}}", 150},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.Local)
			events := `{"kind":"tool_call","tool":"shell","input":{"command":"python -c 'x()'"}}
{"kind":"tool_result","tool":"shell","text":"abcdefgh"}`
			id := "20261014-100000-known"
			writeRun(t, home, id, tt.backend, "ok", events+tt.usage)
			if err := os.WriteFile(filepath.Join(home, "runs", id, "result.json"), []byte(tt.result), 0600); err != nil {
				t.Fatal(err)
			}
			before, err := measure(home, 30, now)
			if err != nil {
				t.Fatal(err)
			}
			if before.InputTokens != tt.want || before.Shapes[4].InputShare == nil || *before.Shapes[4].InputShare != 2/float64(tt.want) {
				t.Fatalf("tokens %d, shape %+v", before.InputTokens, before.Shapes[4])
			}
			writeRun(t, home, "20261014-100001-unknown", tt.backend, "ok", events)
			after, err := measure(home, 30, now)
			if err != nil {
				t.Fatal(err)
			}
			if after.ToolResultBytes != 16 || after.Shapes[4].Bytes != 16 || *after.Shapes[4].InputShare != *before.Shapes[4].InputShare || *after.InlineShare != *before.InlineShare || *after.RepeatedShare != *before.InlineShare {
				t.Fatalf("uncovered bytes changed token shares: %+v", after.stats)
			}
			if *after.Shapes[4].ReplayedShare != *before.Shapes[4].ReplayedShare || *after.InlineReplayedShare != *before.InlineReplayedShare || *after.RepeatedReplayedShare != *before.InlineReplayedShare {
				t.Fatal("uncovered bytes changed replayed token shares")
			}
			data, err := json.Marshal(after)
			if err != nil || !strings.Contains(string(data), `"with_tokens":1`) {
				t.Fatalf("coverage %s %v", data, err)
			}
		})
	}
}

func TestOptionsRejectBadFlagsAndDefaultToTheHRNHome(t *testing.T) {
	for _, args := range [][]string{{"--days", "0"}, {"--days", "x"}, {"--home"}, {"--text"}, {"--json", "extra"}} {
		if _, _, err := options(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	t.Setenv("HRN_HOME", "fixture")
	home, days, err := options([]string{"--json"})
	if err != nil || home != "fixture" || days != 30 {
		t.Fatal(home, days, err)
	}
	home, days, err = options([]string{"--home", "override", "--days", "2", "--json"})
	if err != nil || home != "override" || days != 2 {
		t.Fatal(home, days, err)
	}
	t.Setenv("HRN_HOME", "")
	base, e := os.UserHomeDir()
	if e != nil {
		t.Fatal(e)
	}
	home, _, err = options(nil)
	if err != nil || home != filepath.Join(base, ".hrn") {
		t.Fatal("default home", err)
	}
}

func TestRecordEnvelopes(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.Local)
	writeRun(t, home, "20260901-100000-wrapped", "", "", "{\"kind\":\"tool_result\",\"text\":\"{}\"}\n")
	dir := filepath.Join(home, "runs", "20260901-100000-wrapped")
	for _, name := range []string{"task.json", "result.json"} {
		data := `{"backend":"claude","started":"2026-10-14T10:00:00+03:00","task":{"backend":"claude"},"result":{"status":"ok","backend":"claude"}}`
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	r, err := measure(home, 30, now)
	if err != nil || r.Runs.Read != 1 || r.ToolResultBytes != 2 {
		t.Fatalf("envelope: %+v %v", r, err)
	}
}

func TestAccountingDetails(t *testing.T) {
	s := newStats()
	e := []event{
		{Kind: "tool_call", Input: json.RawMessage(`{"cmd":"python3 -c 'print(1)'"}`)},
		{Kind: "tool_call", Input: json.RawMessage(`{"command":"python3 -c 'print(2)'"}`)},
		{Kind: "tool_call", Input: json.RawMessage(`{"command":123}`)},
		{Kind: "tool_result", Text: "é", Tool: "shell"},
	}
	s.add(e, nil, 0)
	s.finish()
	if len(s.InlineScripts) != 1 || s.InlineScripts[0].Count != 2 || s.InlineScripts[0].Runs != 1 || s.RepeatedBytes != 42 || s.ToolResultBytes != 2 || s.Shapes[4].Bytes != 2 {
		t.Fatalf("accounting %+v", s)
	}
}

func TestShellCommandText(t *testing.T) {
	for _, input := range []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(`null`)} {
		s := newStats()
		command := `python -c 'print(1)'`
		s.add([]event{
			{Kind: "tool_call", Tool: "shell", Text: command, Input: input},
			{Kind: "tool_call", Tool: "read_file", Text: command, Input: input},
			{Kind: "tool_call", Tool: "shell", Text: command, Input: json.RawMessage(`{"command":"echo hello"}`)},
			{Kind: "tool_call", Tool: "shell", Text: command, Input: json.RawMessage(`{"cmd":"echo hello"}`)},
		}, nil, 0)
		s.finish()
		if len(s.InlineScripts) != 1 || s.InlineScripts[0].Count != 1 || s.InlineBytes != int64(len(command)) {
			t.Fatalf("text fallback for input %s: %+v", input, s)
		}
	}
}

func TestAnonymousResults(t *testing.T) {
	for _, tt := range []struct {
		name     string
		events   []event
		terminal int64
	}{
		{"sequential", []event{{Kind: "tool_call", Tool: "Bash"}, {Kind: "tool_result", Text: "hello"}}, 5},
		{"shell batch", []event{{Kind: "tool_call", Tool: "Bash"}, {Kind: "tool_call", Tool: "shell"}, {Kind: "tool_result", Text: "hi"}, {Kind: "tool_result", Text: "bye"}}, 5},
		{"mixed batch", []event{{Kind: "tool_call", Tool: "read_file"}, {Kind: "tool_call", Tool: "Bash"}, {Kind: "tool_result", Text: "hi"}, {Kind: "tool_result", Text: "bye"}}, 5},
		{"named result", []event{{Kind: "tool_call", Tool: "read_file"}, {Kind: "tool_call", Tool: "Bash"}, {Kind: "tool_result", Tool: "Bash", Text: "hi"}, {Kind: "tool_result", Text: "bye"}}, 5},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newStats()
			s.add(tt.events, nil, 0)
			if s.Shapes[4].Bytes != tt.terminal {
				t.Fatalf("terminal %d want %d", s.Shapes[4].Bytes, tt.terminal)
			}
		})
	}
}

func TestUnreadableAndClaudeFilter(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.Local)
	writeRun(t, home, "20261014-100000-no-calls", "claude", "ok", `{"kind":"tool_result","text":"{}"}`)
	writeRun(t, home, "20261014-100001-bad-task", "api", "ok", "")
	dir := filepath.Join(home, "runs", "20261014-100001-bad-task")
	if err := os.WriteFile(filepath.Join(dir, "task.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	writeRun(t, home, "20261014-100002-bad-result", "api", "ok", "")
	if err := os.WriteFile(filepath.Join(home, "runs", "20261014-100002-bad-result", "result.json"), []byte("{} {}"), 0600); err != nil {
		t.Fatal(err)
	}
	writeRun(t, home, "20261014-100003-missing-result", "api", "ok", "")
	if err := os.Remove(filepath.Join(home, "runs", "20261014-100003-missing-result", "result.json")); err != nil {
		t.Fatal(err)
	}
	r, err := measure(home, 30, now)
	if err != nil || r.Runs.Read != 1 || r.Runs.Unreadable != 2 || r.Claude.ToolResultBytes != 0 {
		t.Fatalf("filter %+v %v", r, err)
	}
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDefinitionsCompareTheToolsWithTheMedianCallPerBackend(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.Local)
	usage := func(in, read, write int) string {
		return fmt.Sprintf(`{"kind":"usage","usage":{"input_tokens":%d,"cache_read_tokens":%d,"cache_write_tokens":%d}}`+"\n", in, read, write)
	}
	writeFiles(t, filepath.Join(home, "runs", "20261014-100000-api"), map[string]string{
		"task.json": `{"backend":"api","prompt":"private","mcp":["secret.mcp.json"]}`, "result.json": `{"status":"ok"}`,
		"events.jsonl": usage(100, 0, 2900) + usage(100, 2900, 0) + usage(100, 3900, 0)})
	writeFiles(t, filepath.Join(home, "runs", "20261014-100001-api"), map[string]string{
		"task.json": `{"backend":"api"}`, "result.json": `{"status":"ok","input_tokens":1000,"cache_read_tokens":5000,"cache_write_tokens":3000,"turns":3}`,
		"events.jsonl": ""})
	writeFiles(t, filepath.Join(home, "runs", "20261014-100002-codex"), map[string]string{
		"task.json": `{"backend":"codex"}`, "result.json": `{"status":"ok"}`, "events.jsonl": usage(50000, 99999, 99999)})
	writeFiles(t, filepath.Join(home, "runs", "20261014-100004-codex-totals"), map[string]string{
		"task.json": `{"backend":"codex"}`, "result.json": `{"status":"ok","input_tokens":60000,"cache_read_tokens":80000,"cache_write_tokens":90000,"turns":2}`, "events.jsonl": ""})
	writeFiles(t, filepath.Join(home, "runs", "20261014-100003-api-no-figures"), map[string]string{
		"task.json": `{"backend":"api"}`, "result.json": `{"status":"ok","turns":4}`, "events.jsonl": ""})
	writeFiles(t, filepath.Join(home, "runs", "20261014-100005-api-no-turns"), map[string]string{
		"task.json": `{"backend":"api"}`, "result.json": `{"status":"ok","cache_write_tokens":7000}`, "events.jsonl": ""})
	r, err := measure(home, 30, now)
	if err != nil {
		t.Fatal(err)
	}
	d, err := definitions(r, 30)
	if err != nil {
		t.Fatal(err)
	}
	// api calls: 3000, 3000, 4000 from usage and three of 3000 from totals; codex counts input only.
	if d.API.Runs != 2 || d.API.UsageRuns != 1 || d.API.Calls != 6 || *d.API.Tokens != 3000 {
		t.Fatalf("api %+v", d.API)
	}
	if d.All.Runs != 4 || d.All.Calls != 9 || *d.All.Tokens != 3000 || slices.Max(r.calls.all) != 50000 {
		t.Fatalf("all %+v", d.All)
	}
	if d.MCPDefinitions != "not recorded" || d.APIRunsWithMCP != 1 {
		t.Fatalf("mcp %q %d", d.MCPDefinitions, d.APIRunsWithMCP)
	}
	want, err := agent.NewAPIBackend().ToolDefinitions(agent.Task{Options: agent.Options{Mode: agent.ModeYolo, ShapeTools: true}})
	if err != nil {
		t.Fatal(err)
	}
	shaping := d.Definitions["shaping"]
	if shaping.Bytes != len(want) || shaping.Tokens != float64(len(want))/4 || *shaping.CallShare != shaping.Tokens/3000 {
		t.Fatalf("shaping %+v", shaping)
	}
	if d.Definitions["readonly"].Tools != 3 || d.Definitions["auto"].Tools != 6 || d.Definitions["yolo"].Tools != 6 || shaping.Tools != 7 {
		t.Fatalf("tool counts %+v", d.Definitions)
	}
	if len(d.Largest) != 5 || !slices.ContainsFunc(d.Largest, func(s toolSize) bool { return s.Tool == "read_original" }) || !sort.SliceIsSorted(d.Largest, func(i, j int) bool { return d.Largest[i].Bytes > d.Largest[j].Bytes }) {
		t.Fatalf("largest %+v", d.Largest)
	}
	data, _ := json.Marshal(d)
	var fields struct {
		API struct {
			Runs   int   `json:"runs_with_cache_write"`
			Tokens int64 `json:"cache_write_tokens"`
		} `json:"api"`
	}
	if err := json.Unmarshal(data, &fields); err != nil || fields.API.Runs != 2 || fields.API.Tokens != 5900 {
		t.Fatalf("cache writes %s: %v", data, err)
	}
	if strings.Contains(string(data), "secret") || strings.Contains(string(data), "private") || strings.Contains(string(data), home) {
		t.Fatal("record text leaked")
	}
}

func TestDefinitionsLeaveTheShareUnsetWithoutApiCalls(t *testing.T) {
	home := t.TempDir()
	writeFiles(t, filepath.Join(home, "runs"), nil)
	r, err := measure(home, 30, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	d, err := definitions(r, 30)
	if err != nil {
		t.Fatal(err)
	}
	if d.API.Tokens != nil || d.Definitions["auto"].CallShare != nil || d.Definitions["auto"].Bytes == 0 {
		t.Fatalf("%+v", d)
	}
}

func TestDefinitionsFlagIsTakenOutBeforeTheSharedOptions(t *testing.T) {
	on, rest := definitionsFlag([]string{"--home", "h", "--definitions", "--days", "2"})
	if !on || strings.Join(rest, " ") != "--home h --days 2" {
		t.Fatal(on, rest)
	}
	if on, _ := definitionsFlag([]string{"--json"}); on {
		t.Fatal("flag found where none was given")
	}
}
