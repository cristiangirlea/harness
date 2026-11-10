package agent

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/tools"
)

func isFakeGate() bool {
	return strings.TrimSuffix(strings.ToLower(filepath.Base(os.Args[0])), ".exe") == tools.GateName
}

// fakeGate stands in for adanext-exec: it answers the handshake, logs each run's arguments next
// to itself and runs the script through the shell.
func fakeGate(args []string) int {
	exe, err := os.Executable()
	if err != nil || len(args) == 0 {
		return 126
	}
	switch args[0] {
	case "check":
		if _, err := os.Stat(filepath.Join(filepath.Dir(exe), "old")); err == nil {
			fmt.Fprintln(os.Stderr, `adanext-exec: unknown command "check"`)
			return 121
		}
		if f, err := os.OpenFile(filepath.Join(filepath.Dir(exe), "checks.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintln(f, strings.Join(args, " "))
			f.Close()
		}
		var sent struct{ Script string }
		_ = json.NewDecoder(os.Stdin).Decode(&sent)
		if strings.HasPrefix(sent.Script, "fake-refuse") {
			fmt.Println(`{"version":"adanext-exec/1","decision":"refuse","rule":"git-push"}`)
			return 0
		}
		fmt.Println(`{"version":"adanext-exec/1","decision":"allow"}`)
		return 0
	case "hook":
		if _, err := os.Stat(filepath.Join(filepath.Dir(exe), "nohook")); err == nil {
			fmt.Fprintln(os.Stderr, `adanext-exec: unknown command "hook"`)
			return 121
		}
		hookArgs, _ := json.Marshal(args)
		_ = os.WriteFile(filepath.Join(filepath.Dir(exe), "hook.args"), hookArgs, 0o644)
		for marker, reason := range map[string]string{"refuses": "adanext-exec: refused under rule \"x\"", "othersrefuse": "someone else"} {
			if _, err := os.Stat(filepath.Join(filepath.Dir(exe), marker)); err == nil {
				answer, _ := json.Marshal(map[string]map[string]string{"hookSpecificOutput": {"permissionDecision": "deny", "permissionDecisionReason": reason}})
				fmt.Println(string(answer))
				return 0
			}
		}
		var in struct {
			Input struct {
				Timeout int `json:"timeout"`
			} `json:"tool_input"`
		}
		_ = json.NewDecoder(os.Stdin).Decode(&in)
		timeout := ""
		if _, err := os.Stat(filepath.Join(filepath.Dir(exe), "notimeout")); err != nil && in.Input.Timeout > 0 {
			timeout = ` '--timeout' '1s'`
		}
		policy := ""
		if i := slices.Index(args, "--policy"); i >= 0 && i+1 < len(args) {
			policy = ` '--policy' ` + tools.PosixQuote(filepath.ToSlash(args[i+1]))
		}
		command, _ := json.Marshal(tools.PosixQuote(filepath.ToSlash(exe)) + ` 'run' '--hook-build' 'ab' '--shell' 'bash'` + policy + timeout + ` '--agent' 'claude-code' '--script-b64' 'Z2l0IHN0YXR1cw'`)
		fmt.Printf(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","permissionDecisionReason":"allowed","updatedInput":{"command":%s}}}`+"\n", command)
		return 0
	case "run":
		line, _ := json.Marshal(args)
		_ = os.WriteFile(filepath.Join(filepath.Dir(exe), "runs.log"), line, 0o644)
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		fs.String("shell", "", "")
		timeout := fs.Duration("timeout", 0, "")
		fs.String("agent", "", "")
		fs.String("run-id", "", "")
		policy := fs.String("policy", "", "")
		if fs.Parse(args[1:]) != nil || len(fs.Args()) != 1 {
			return 121
		}
		seen, _ := os.ReadFile(*policy)
		_ = os.WriteFile(filepath.Join(filepath.Dir(exe), "policy.seen"), seen, 0o644)
		wd, _ := os.Getwd()
		res, err := tools.RunShell(context.Background(), wd, fs.Args()[0], *timeout, nil)
		if err != nil {
			return 126
		}
		os.Stdout.WriteString(res.Output)
		return res.ExitCode
	}
	return 121
}

// installFakeGate copies the test binary under the gate's name first in PATH and returns its directory.
func installFakeGate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	name := tools.GateName
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestCommandGateOptionParsesRoundTripsAndInherits(t *testing.T) {
	var o Options
	fs := flag.NewFlagSet("gate", flag.ContinueOnError)
	BindFlags(fs, &o)
	if err := fs.Parse([]string{"--gate"}); err != nil || !o.Gate {
		t.Fatalf("--gate: %v", err)
	}
	b, err := json.Marshal(o)
	if err != nil || !strings.Contains(string(b), `"command_gate":true`) {
		t.Fatalf("record: %s %v", b, err)
	}
	var back Options
	if err := json.Unmarshal(b, &back); err != nil || !back.Gate {
		t.Fatalf("round trip: %+v %v", back, err)
	}
	if b, _ := json.Marshal(Options{}); strings.Contains(string(b), "gate") {
		t.Fatalf("a run without the gate records it: %s", b)
	}
	if !(Options{}).Over(o).Gate {
		t.Fatal("a task did not inherit the spec's gate")
	}
	if schema := o.Schema(); schema["command_gate"] == nil || schema["gate"] != nil {
		t.Fatalf("the MCP schema names the option wrongly: %v", schema["command_gate"])
	}
}

func TestAPIRunWithTheGateDoesNotStartWithoutIt(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	res, _, fake := runFake(t, [][]string{textTurn("done")},
		Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Mode: ModeYolo, Gate: true}})
	if res.Status != StatusError || !strings.Contains(res.Error, "adanext-exec is not in PATH") {
		t.Fatalf("result = %s %q, want an error naming the missing gate", res.Status, res.Error)
	}
	if fake.served != 0 {
		t.Fatalf("the model was called %d times before the gate was found", fake.served)
	}
}

func TestAPIRunWithAnOlderGateDoesNotStart(t *testing.T) {
	dir := installFakeGate(t)
	if err := os.WriteFile(filepath.Join(dir, "old"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	res, _, fake := runFake(t, [][]string{textTurn("done")},
		Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Mode: ModeYolo, Gate: true}})
	if res.Status != StatusError || !strings.Contains(res.Error, "handshake") {
		t.Fatalf("result = %s %q, want a failed handshake", res.Status, res.Error)
	}
	if fake.served != 0 {
		t.Fatalf("the model was called %d times with an older gate", fake.served)
	}
}

func TestAPIRunWithTheGateRunsCommandsThroughIt(t *testing.T) {
	gateDir := installFakeGate(t)
	runDir := filepath.Join(t.TempDir(), "r-gated")
	if err := os.Mkdir(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	res, events, fake := runFake(t, [][]string{
		toolTurn("toolu_c", "run_command", `{"command":"echo through-the-gate"}`),
		textTurn("done"),
	}, Task{Prompt: "go", Dir: t.TempDir(), RunDir: runDir, Options: Options{Mode: ModeYolo, Gate: true}})
	if res.Status != StatusOK {
		t.Fatalf("result = %s %q", res.Status, res.Error)
	}
	block := lastToolResult(t, fake.requests[1])
	if block["is_error"] == true || !strings.Contains(fmt.Sprint(block["content"]), "through-the-gate") {
		t.Fatalf("tool result = %v", block)
	}
	logged, err := os.ReadFile(filepath.Join(gateDir, "runs.log"))
	if err != nil || !strings.Contains(string(logged), `"--run-id","r-gated","--","echo through-the-gate"`) {
		t.Fatalf("the gate ran %s (%v), want the command with the run id", logged, err)
	}
	if sys := systemText(t, fake.requests[0]); !strings.Contains(sys[len(sys)-1], "Command gate: on") {
		t.Errorf("the environment block does not say the gate is on:\n%s", sys[len(sys)-1])
	}
	said := false
	for _, e := range events {
		said = said || (e.Kind == EventInfo && strings.HasPrefix(e.Text, "gate: run_command runs through "))
	}
	if !said {
		t.Error("the run does not record which gate its commands ran through")
	}
}

func TestAPIRunWithoutTheGateSaysNothingOfIt(t *testing.T) {
	_, _, fake := runFake(t, [][]string{textTurn("ok")}, Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Mode: ModeYolo}})
	if sys := systemText(t, fake.requests[0]); strings.Contains(sys[len(sys)-1], "Command gate") {
		t.Errorf("the environment block names a gate that is off:\n%s", sys[len(sys)-1])
	}
}

func TestCLIBackendsRefuseTheGate(t *testing.T) {
	for _, b := range []*CLIBackend{NewGeminiBackend(), NewOpencodeBackend(), NewAgyBackend()} {
		res := b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Gate: true}}, func(Event) {})
		if res.Status != StatusError || !strings.Contains(res.Error, "do not run through adanext-exec yet") {
			t.Errorf("%s: result = %s %q, want the gate refused", b.Name(), res.Status, res.Error)
		}
	}
}
