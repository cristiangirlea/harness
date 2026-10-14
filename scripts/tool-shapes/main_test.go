// name: tool-shapes-tests
// summary: Check tool shape classification, normalisation and record accounting.
// effects: read, exec
// parameters: none
// example: go test -count=1 ./scripts/tool-shapes
// hash: 4634e920659c3b45f114be525d2fadbb2d4d781c9f31e25248b7ef25cb6e1e20
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	for _, tt := range []struct{ text, tool, want string }{
		{"go test ./...\nok pkg 1s", "shell", "test"}, {"FAIL pkg", "", "test"}, {"--- PASS: TestX", "", "test"},
		{"2026-10-14T12:00:00Z ready", "", "log"}, {"[INFO] ready", "", "log"}, {"ERROR: failed", "", "log"},
		{`{"a":1}`, "shell", "json"}, {`[1,2]`, "", "json"}, {"a,b\n1,2", "", "table"}, {"a\tb\n1\t2", "", "table"},
		{"a,b\n1,2,3", "shell", "terminal"}, {"hello", "Bash", "terminal"}, {"hello", "exec_command", "terminal"},
		{"hello", "read_file", "other"}, {"", "", "other"},
	} {
		t.Run(tt.want+tt.text, func(t *testing.T) {
			if got := classify(tt.text, tt.tool); got != tt.want {
				t.Fatalf("got %s want %s", got, tt.want)
			}
		})
	}
}

func TestInline(t *testing.T) {
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

func TestMeasure(t *testing.T) {
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

func TestOptions(t *testing.T) {
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
	s.add(e)
	s.finish()
	if len(s.InlineScripts) != 1 || s.InlineScripts[0].Count != 2 || s.InlineScripts[0].Runs != 1 || s.RepeatedBytes != 42 || s.ToolResultBytes != 2 || s.Shapes[4].Bytes != 2 {
		t.Fatalf("accounting %+v", s)
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
		{"mixed batch", []event{{Kind: "tool_call", Tool: "read_file"}, {Kind: "tool_call", Tool: "Bash"}, {Kind: "tool_result", Text: "hi"}, {Kind: "tool_result", Text: "bye"}}, 0},
		{"named result", []event{{Kind: "tool_call", Tool: "read_file"}, {Kind: "tool_call", Tool: "Bash"}, {Kind: "tool_result", Tool: "Bash", Text: "hi"}, {Kind: "tool_result", Text: "bye"}}, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newStats()
			s.add(tt.events)
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
