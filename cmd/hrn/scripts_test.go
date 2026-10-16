package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/scripthint"
)

// hintRepo makes a repository whose scripts/ holds this repository's real
// kept scripts named, so the shipped matches: lines are what is tested.
func hintRepo(t *testing.T, extra map[string]string, real ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{".git", "scripts"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range real {
		b, err := os.ReadFile(filepath.Join("..", "..", "scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		extra[name] = string(b)
	}
	for name, body := range extra {
		if err := os.WriteFile(filepath.Join(root, "scripts", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func hintPayload(t *testing.T, tool, command, cwd string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"session_id":      "s",
		"hook_event_name": "PreToolUse",
		"tool_name":       tool,
		"tool_input":      map[string]string{"command": command, "description": "d"},
		"cwd":             cwd,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// runHint runs the hook on stdin and returns its stdout and the hints log.
func runHint(t *testing.T, stdin string) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	var out bytes.Buffer
	code, err := scriptsHint(strings.NewReader(stdin), &out, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if code != 0 || err != nil {
		t.Fatalf("exit %d, %v", code, err)
	}
	log, err := os.ReadFile(filepath.Join(home, "script-hints.jsonl"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return out.String(), string(log)
}

const heredocPortPoll = "python3 - <<'PY'\nimport socket, time\nfor _ in range(50):\n    try:\n        socket.create_connection(('127.0.0.1', 8089), 1).close()\n        break\n    except OSError:\n        time.sleep(0.2)\nPY"

func TestScriptsHintNamesWaitForForAHeredocPortPoll(t *testing.T) {
	root := hintRepo(t, map[string]string{}, "test-summary.sh", "wait-for.sh")
	out, log := runHint(t, hintPayload(t, "Bash", heredocPortPoll, root))
	want := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":"scripts/wait-for.sh covers this inline script; parameters: host:string=required; port:int=required; timeout:seconds=10."}}` + "\n"
	if out != want {
		t.Fatalf("stdout\n got %s\nwant %s", out, want)
	}
	var line struct{ Time, Script, Repo string }
	if strings.Count(log, "\n") != 1 || json.Unmarshal([]byte(log), &line) != nil {
		t.Fatalf("log = %q, want one JSON line", log)
	}
	if line.Time != "2026-01-02T03:04:05Z" || line.Script != "scripts/wait-for.sh" || line.Repo == "" {
		t.Fatalf("log line = %+v", line)
	}
}

func TestScriptsHintIsSilentWithoutAMatch(t *testing.T) {
	root := hintRepo(t, map[string]string{}, "wait-for.sh")
	out, log := runHint(t, hintPayload(t, "Bash", "python3 -c 'print(1+1)'", root))
	if out != "" || log != "" {
		t.Fatalf("stdout %q, log %q", out, log)
	}
}

func TestScriptsHintIsSilentOnInputItDoesNotUnderstand(t *testing.T) {
	root := hintRepo(t, map[string]string{}, "wait-for.sh")
	for name, stdin := range map[string]string{
		"malformed": `{"tool_name": "Bash", `,
		"empty":     "",
		"edit":      hintPayload(t, "Edit", heredocPortPoll, root),
		"oversized": hintPayload(t, "Bash", heredocPortPoll+"\n#"+strings.Repeat("x", 1<<20), root),
	} {
		if out, log := runHint(t, stdin); out != "" || log != "" {
			t.Errorf("%s: stdout %q, log %q", name, out, log)
		}
	}
}

func TestScriptsHintNamesWaitForForAPowerShellCommandBlock(t *testing.T) {
	root := hintRepo(t, map[string]string{}, "wait-for.sh")
	command := `pwsh -NoProfile -Command { while (-not (Test-NetConnection 127.0.0.1 -Port 8089).TcpTestSucceeded) { Start-Sleep 1 }; 'up' }`
	out, _ := runHint(t, hintPayload(t, "PowerShell", command, root))
	if !strings.Contains(out, `"additionalContext":"scripts/wait-for.sh covers this inline script;`) {
		t.Fatalf("stdout %q", out)
	}
	if strings.Contains(out, "permissionDecision") {
		t.Fatalf("the hint decided a permission: %s", out)
	}
}

func TestScriptsHintIsSilentForACommandWithNoInlineScript(t *testing.T) {
	root := hintRepo(t, map[string]string{}, "wait-for.sh", "test-summary.sh")
	if out, log := runHint(t, hintPayload(t, "Bash", "go test ./...", root)); out != "" || log != "" {
		t.Fatalf("stdout %q, log %q", out, log)
	}
}

func TestScriptsHintIsSilentWhenTheCommandAlreadyCallsWaitFor(t *testing.T) {
	root := hintRepo(t, map[string]string{}, "wait-for.sh")
	command := "bash scripts/wait-for.sh 127.0.0.1 8089 && bash -c 'exec 3<>/dev/tcp/127.0.0.1/8089'"
	if out, log := runHint(t, hintPayload(t, "Bash", command, root)); out != "" || log != "" {
		t.Fatalf("stdout %q, log %q", out, log)
	}
}

func TestScriptsHintSkipsAnInvalidPatternAndStillMatchesTheNextScript(t *testing.T) {
	root := hintRepo(t, map[string]string{
		"a-broken.sh": "#!/usr/bin/env bash\n# name: a-broken\n# parameters: none\n# matches: socket(\n",
	}, "wait-for.sh")
	out, _ := runHint(t, hintPayload(t, "Bash", heredocPortPoll, root))
	if !strings.Contains(out, "scripts/wait-for.sh covers") {
		t.Fatalf("stdout %q", out)
	}
}

func TestScriptsHintFindsTheRootFromASubdirectory(t *testing.T) {
	root := hintRepo(t, map[string]string{}, "wait-for.sh")
	sub := filepath.Join(root, "cmd", "hrn")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	out, log := runHint(t, hintPayload(t, "Bash", heredocPortPoll, sub))
	if !strings.Contains(out, "scripts/wait-for.sh covers") || log == "" {
		t.Fatalf("stdout %q, log %q", out, log)
	}
}

// Claude Code reads exit 2 from a PreToolUse hook as a block.
func TestScriptsHintRejectsAnUnknownSubcommandWithoutBlocking(t *testing.T) {
	if code, err := cmdScripts([]string{"list"}); code != 1 || err == nil {
		t.Fatalf("exit %d, %v", code, err)
	}
}

// The shipped matches: lines, against inline scripts each should and should not claim.
func TestScriptHeadersMatchTheInlineScriptsTheyCover(t *testing.T) {
	scripts := scripthint.Load(filepath.Join("..", ".."))
	byFile := map[string]scripthint.Script{}
	for _, s := range scripts {
		byFile[s.File] = s
	}
	for _, tc := range []struct {
		file, inline string
		want         bool
	}{
		{"wait-for.sh", "import socket; socket.create_connection(('localhost', 5432), 1)", true},
		{"wait-for.sh", "s = socket.socket(); r = s.connect_ex(('127.0.0.1', 80))", true},
		{"wait-for.sh", `c, err := net.Dial("tcp", "127.0.0.1:8080")`, true},
		{"wait-for.sh", `conn, err := net.DialTimeout("tcp", addr, time.Second)`, true},
		{"wait-for.sh", "until nc -z localhost 5432; do sleep 1; done", true},
		{"wait-for.sh", "until nc -w 1 -z localhost 5432; do sleep 1; done", true},
		{"wait-for.sh", "until (exec 3<>/dev/tcp/127.0.0.1/8089) 2>/dev/null; do sleep 1; done", true},
		{"wait-for.sh", "while (-not (Test-NetConnection localhost -Port 80).TcpTestSucceeded) { sleep 1 }", true},
		{"wait-for.sh", "require('net').createConnection(8080, '127.0.0.1').on('connect', () => process.exit(0))", true},
		{"wait-for.sh", "import json; print(json.load(open('x.json')))", false},
		{"wait-for.sh", "nc -l 8080", false},
		{"test-summary.sh", `for l in sys.stdin: e = json.loads(l); c[e["Action"]] += 1 if e.get("Action") in ("pass", "fail") else 0`, true},
		{"test-summary.sh", `counts = {"pass": 0, "fail": 0}; a = ev['Action']`, true},
		{"test-summary.sh", `if e.Action == "fail" { failed++ }`, true},
		{"test-summary.sh", `jq -r 'select(.Action=="fail") | .Test'`, true},
		{"test-summary.sh", `print(e["Action"])`, false},
		{"test-summary.sh", `if result == "pass": print("ok")`, false},
	} {
		s, ok := byFile[tc.file]
		if !ok {
			t.Fatalf("scripts/%s has no matches: line", tc.file)
		}
		if got := s.Matches.MatchString(scripthint.Normalize(tc.inline)); got != tc.want {
			t.Errorf("%s matches %q = %v, want %v", tc.file, tc.inline, got, tc.want)
		}
	}
}
