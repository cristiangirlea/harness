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

var codexGateStreams = map[string][]string{
	"routed": {
		`{"type":"thread.started","thread_id":"019a0000-0000-7000-8000-000000000003"}`,
		`{"type":"item.started","item":{"id":"i1","type":"mcp_tool_call","server":"hrn_gate","tool":"run_command","arguments":{"command":"echo hi"},"status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"i1","type":"mcp_tool_call","server":"hrn_gate","tool":"run_command","arguments":{"command":"echo hi"},"result":{"content":[{"type":"text","text":"hi\n[exit code 0]"}]},"status":"completed"}}`,
		`{"type":"item.started","item":{"id":"i2","type":"mcp_tool_call","server":"hrn_gate","tool":"run_command","arguments":{"command":"git push"},"status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"i2","type":"mcp_tool_call","server":"hrn_gate","tool":"run_command","arguments":{"command":"git push"},"error":{"message":"refused"},"status":"failed"}}`,
		`{"type":"item.started","item":{"id":"i3","type":"mcp_tool_call","server":"other","tool":"run_command","arguments":{"command":"not the gate"},"status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"i4","type":"agent_message","text":"done"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1}}`,
	},
	"unrouted": {
		`{"type":"thread.started","thread_id":"019a0000-0000-7000-8000-000000000003"}`,
		`{"type":"item.started","item":{"id":"i1","type":"command_execution","command":"git status","status":"in_progress"}}`,
	},
	"unknown": {
		`{"type":"thread.started","thread_id":"019a0000-0000-7000-8000-000000000003"}`,
		`{"type":"item.started","item":{"id":"i1","type":"script_runner_call","script":"run()","status":"in_progress"}}`,
	},
}

// TestHelperCLICodexGate is not a test: the test binary acts as the codex CLI and records its arguments.
func TestHelperCLICodexGate(t *testing.T) {
	stream := os.Getenv("HRN_CODEX_GATE_STREAM")
	if stream == "" {
		t.Skip("helper")
	}
	line, _ := json.Marshal(os.Args[slices.Index(os.Args, "--")+1:])
	_ = os.WriteFile(os.Getenv("HRN_CODEX_GATE_LOG"), line, 0o644)
	for _, l := range codexGateStreams[stream] {
		fmt.Println(l)
	}
	if stream == "unrouted" || stream == "unknown" {
		time.Sleep(30 * time.Second)
	}
	os.Exit(0)
}

func runGatedCodex(t *testing.T, stream string, task Task) (Result, []Event, []string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "started.json")
	t.Setenv("HRN_CODEX_GATE_STREAM", stream)
	t.Setenv("HRN_CODEX_GATE_LOG", log)
	b := NewCodexBackend()
	real := b.args
	b.name, b.candidates = os.Args[0], nil
	b.args = func(task Task, prompt string) []string {
		return append([]string{"-test.run=^TestHelperCLICodexGate$", "--"}, real(task, prompt)...)
	}
	task.Prompt = "go"
	if task.Dir == "" {
		task.Dir = t.TempDir()
	}
	var events []Event
	res := b.Run(context.Background(), task, func(e Event) { events = append(events, e) })
	data, err := os.ReadFile(log)
	if err != nil {
		return res, events, nil
	}
	var args []string
	if err := json.Unmarshal(data, &args); err != nil {
		t.Fatal(err)
	}
	return res, events, args
}

func codexGateConfig(args []string, field string) string {
	prefix := "mcp_servers." + codexGateServer + "." + field + "="
	for _, a := range args {
		if value, ok := strings.CutPrefix(a, prefix); ok {
			return value
		}
	}
	return ""
}

func TestGatedCodexRunsWithItsCodeToolsOffAndTheGatesToolServed(t *testing.T) {
	installFakeGate(t)
	res, _, args := runGatedCodex(t, "routed", Task{RunDir: "RUN-DIR", Options: Options{Mode: ModeYolo, Gate: true}})
	if res.Status == StatusError || args == nil {
		t.Fatalf("result = %s %q, args %v", res.Status, res.Error, args)
	}
	disabled := map[string]bool{}
	for i := 0; i+1 < len(args)-1; i++ {
		if args[i] == "--disable" {
			disabled[args[i+1]] = true
		}
	}
	for _, feature := range []string{"shell_tool", "unified_exec", "computer_use", "browser_use", "apps", "plugins"} {
		if !disabled[feature] {
			t.Errorf("%s is not disabled: %v", feature, args)
		}
	}
	// codex reaches MCP tools only through its code host, so turning it off leaves no run_command.
	if slices.Contains(args, "code_mode_host") {
		t.Errorf("code_mode_host is disabled: %v", args)
	}
	if args[len(args)-1] != "-" {
		t.Errorf("the prompt's - is not last: %v", args)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if want := tomlString(self); codexGateConfig(args, "command") != want {
		t.Errorf("command = %s, want %s", codexGateConfig(args, "command"), want)
	}
	server := codexGateConfig(args, "args")
	for _, want := range []string{`"gate-tools"`, `"--gate"`, `"--policy"`, `hrn-gate-policy-`, `"--policy-sha256"`, `"--run-dir"`, `RUN-DIR`} {
		if !strings.Contains(server, want) {
			t.Errorf("server args %s lack %s", server, want)
		}
	}
	if strings.Contains(server, "--allow-dangerous") || strings.Contains(server, "--shape-tools") {
		t.Errorf("server args %s carry options this run did not set", server)
	}
	if !strings.Contains(strings.ToUpper(codexGateConfig(args, "env_vars")), `"PATH"`) {
		t.Errorf("env_vars = %s, want PATH handed to the server", codexGateConfig(args, "env_vars"))
	}
	if got := codexGateConfig(args, "tool_timeout_sec"); got != "660" {
		t.Errorf("tool_timeout_sec = %s, want past run_command's longest timeout", got)
	}
}

func TestGatedCodexPassesTheRunsOptionsToTheTool(t *testing.T) {
	installFakeGate(t)
	_, _, args := runGatedCodex(t, "routed", Task{Options: Options{Mode: ModeYolo, Gate: true, AllowDangerous: true, ShapeTools: true}})
	server := codexGateConfig(args, "args")
	if !strings.Contains(server, "--allow-dangerous") || !strings.Contains(server, "--shape-tools") {
		t.Fatalf("server args = %s", server)
	}
}

func TestGatedCodexRecordsTheGatesToolAsShellCalls(t *testing.T) {
	installFakeGate(t)
	_, events, _ := runGatedCodex(t, "routed", Task{Options: Options{Mode: ModeYolo, Gate: true}})
	var got []string
	for _, e := range events {
		if e.Kind == EventToolCall || e.Kind == EventToolResult {
			got = append(got, fmt.Sprintf("%s %s %q %v", e.Kind, e.Tool, e.Text, e.IsError))
		}
	}
	want := []string{
		fmt.Sprintf("%s shell %q false", EventToolCall, "echo hi"),
		fmt.Sprintf("%s shell %q false", EventToolResult, "hi\n[exit code 0]"),
		fmt.Sprintf("%s shell %q false", EventToolCall, "git push"),
		fmt.Sprintf("%s shell %q true", EventToolResult, "refused"),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("events:\n%q\nwant:\n%q", got, want)
	}
}

func TestGatedCodexStopsARunWhoseCommandSkippedTheTool(t *testing.T) {
	installFakeGate(t)
	started := time.Now()
	res, _, _ := runGatedCodex(t, "unrouted", Task{Options: Options{Mode: ModeYolo, Gate: true}})
	if res.Status != StatusError || !strings.HasPrefix(res.Error, gateUnrouted) {
		t.Fatalf("result = %s %q", res.Status, res.Error)
	}
	if time.Since(started) > 20*time.Second {
		t.Fatal("the run was not stopped")
	}
}

func TestGatedCodexStopsARunThatUsesAToolHrnDoesNotKnow(t *testing.T) {
	installFakeGate(t)
	started := time.Now()
	res, _, _ := runGatedCodex(t, "unknown", Task{Options: Options{Mode: ModeYolo, Gate: true}})
	if res.Status != StatusError || !strings.HasPrefix(res.Error, gateUnrouted) || !strings.Contains(res.Error, "script_runner_call") {
		t.Fatalf("result = %s %q", res.Status, res.Error)
	}
	if time.Since(started) > 20*time.Second {
		t.Fatal("the run was not stopped")
	}
}

func TestGatedCodexRefusesAConfiguredServerWithTheGatesName(t *testing.T) {
	installFakeGate(t)
	toml := []byte("[mcp_servers.hrn_gate]\ncommand = \"x\"\n")
	for _, where := range []string{"task", "user", "project"} {
		t.Run(where, func(t *testing.T) {
			home, dir := t.TempDir(), t.TempDir()
			t.Setenv("CODEX_HOME", home)
			task := Task{Dir: dir, Options: Options{Mode: ModeYolo, Gate: true}}
			var path string
			var data []byte
			switch where {
			case "task":
				path, data = filepath.Join(t.TempDir(), "mcp.json"), []byte(`{"mcpServers":{"hrn_gate":{"command":"x"}}}`)
				task.MCP = []string{path}
			case "user":
				path, data = filepath.Join(home, "config.toml"), toml
			case "project":
				path, data = filepath.Join(dir, ".codex", "config.toml"), toml
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			res, _, args := runGatedCodex(t, "routed", task)
			if res.Status != StatusError || !strings.Contains(res.Error, "hrn_gate is configured") || args != nil {
				t.Fatalf("result = %s %q, started with %v", res.Status, res.Error, args)
			}
		})
	}
}

func TestGatedCodexGivesTheServerAnAbsoluteDirectory(t *testing.T) {
	installFakeGate(t)
	parent := t.TempDir()
	if err := os.Mkdir(filepath.Join(parent, "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(parent)
	_, _, args := runGatedCodex(t, "routed", Task{Dir: "run", Options: Options{Mode: ModeYolo, Gate: true}})
	var server []string
	if err := json.Unmarshal([]byte(codexGateConfig(args, "args")), &server); err != nil {
		t.Fatal(err)
	}
	i := slices.Index(server, "--dir")
	if i < 0 {
		t.Fatalf("server args = %v", server)
	}
	got, err := filepath.EvalSymlinks(server[i+1])
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(parent, "run"))
	if err != nil || got != want || !filepath.IsAbs(server[i+1]) {
		t.Fatalf("--dir = %s, want %s (%v)", server[i+1], want, err)
	}
}

func TestGatedCodexKeepsItsOptionsAheadOfAFork(t *testing.T) {
	installFakeGate(t)
	session := "019a0000-0000-7000-8000-000000000003"
	_, _, args := runGatedCodex(t, "routed", Task{ForkSession: session, Options: Options{Mode: ModeYolo, Gate: true}})
	fork, disable := slices.Index(args, "fork"), slices.Index(args, "--disable")
	if fork < 0 || disable < 0 || disable > fork || args[fork+1] != session || args[len(args)-1] != "-" {
		t.Fatalf("args = %v", args)
	}
}

func TestGatedCodexNeedsYolo(t *testing.T) {
	installFakeGate(t)
	for _, mode := range []Mode{ModeReadOnly, ModeAsk, ModeAuto} {
		res, _, args := runGatedCodex(t, "routed", Task{Options: Options{Mode: mode, Gate: true}})
		if res.Status != StatusError || !strings.Contains(res.Error, "--mode yolo") || args != nil {
			t.Errorf("%s: result = %s %q, started with %v", mode, res.Status, res.Error, args)
		}
	}
}

func TestGatedCodexDoesNotStartWithoutTheGate(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	res, _, args := runGatedCodex(t, "routed", Task{Options: Options{Mode: ModeYolo, Gate: true}})
	if res.Status != StatusError || !strings.Contains(res.Error, "not in PATH") || args != nil {
		t.Fatalf("result = %s %q, started with %v", res.Status, res.Error, args)
	}
}

func TestCodexGateEnvNamesLeaveOutWhatTheRunWithholds(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "x")
	t.Setenv("UNLISTED_TEST_VARIABLE", "x")
	names := codexGateEnvNames(Task{Options: Options{}})
	for _, withheld := range []string{"ANTHROPIC_API_KEY", "UNLISTED_TEST_VARIABLE"} {
		if slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(n, withheld) }) {
			t.Errorf("%s is handed to the server: %v", withheld, names)
		}
	}
	names = codexGateEnvNames(Task{Options: Options{InheritEnv: true}})
	if !slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(n, "UNLISTED_TEST_VARIABLE") }) {
		t.Errorf("--inherit-env did not keep the variable: %v", names)
	}
	if slices.ContainsFunc(names, func(n string) bool { return n == "" || strings.HasPrefix(n, "=") }) {
		t.Errorf("a name codex could not read: %v", names)
	}
}

func TestCodexGateEnvNamesKeepTheRunsChildEnvAndWithholdTokens(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	names := codexGateEnvNames(Task{ChildEnv: []string{"HRN_CHILD_ONLY=1"}})
	if !slices.Contains(names, "HRN_CHILD_ONLY") {
		t.Errorf("the run's child variable is missing: %v", names)
	}
	if slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(n, "GITHUB_TOKEN") }) {
		t.Errorf("a token is handed to the server: %v", names)
	}
}

func TestCodexGateEnvNamesPassThroughACmdShim(t *testing.T) {
	names := codexGateEnvNames(Task{ChildEnv: []string{"ProgramFiles(x86)=C:\\x", "CommonProgramFiles(x86)=C:\\y", "PLAIN_NAME=1"}})
	if slices.ContainsFunc(names, func(n string) bool { return strings.ContainsAny(n, "()") }) || !slices.Contains(names, "PLAIN_NAME") {
		t.Fatalf("names = %v", names)
	}
	arg := "mcp_servers." + codexGateServer + ".env_vars=" + tomlArray(names)
	if err := codexBatchArgsError("codex.cmd", []string{"-c", arg}); err != nil {
		t.Fatalf("a cmd.exe shim refuses the names: %v", err)
	}
}
