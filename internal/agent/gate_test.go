package agent

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
		fmt.Println(`{"version":"adanext-exec/1","decision":"allow"}`)
		return 0
	case "run":
		line, _ := json.Marshal(args)
		_ = os.WriteFile(filepath.Join(filepath.Dir(exe), "runs.log"), line, 0o644)
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		fs.String("shell", "", "")
		timeout := fs.Duration("timeout", 0, "")
		fs.String("agent", "", "")
		fs.String("run-id", "", "")
		if fs.Parse(args[1:]) != nil || len(fs.Args()) != 1 {
			return 121
		}
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
	for _, b := range []*CLIBackend{NewClaudeBackend(), NewCodexBackend(), NewGeminiBackend(), NewOpencodeBackend(), NewAgyBackend()} {
		res := b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Gate: true}}, func(Event) {})
		if res.Status != StatusError || !strings.Contains(res.Error, "do not run through adanext-exec yet") {
			t.Errorf("%s: result = %s %q, want the gate refused", b.Name(), res.Status, res.Error)
		}
	}
}
