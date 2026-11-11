package hrnmcp

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/tools"
)

func isFakeGate() bool {
	return strings.TrimSuffix(strings.ToLower(filepath.Base(os.Args[0])), ".exe") == tools.GateName
}

// fakeGate stands in for adanext-exec: it answers the handshake, marks that it ran a command and runs it.
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
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		fs.String("shell", "", "")
		timeout := fs.Duration("timeout", 0, "")
		fs.String("agent", "", "")
		fs.String("run-id", "", "")
		fs.String("policy", "", "")
		if fs.Parse(args[1:]) != nil || len(fs.Args()) != 1 {
			return 121
		}
		_ = os.WriteFile(filepath.Join(filepath.Dir(exe), "ran"), []byte(fs.Args()[0]), 0o644)
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

// recordGate swaps the stub backend for one that notes whether each task asked for the gate.
func recordGate(t *testing.T) func() []bool {
	t.Helper()
	orig, err := agent.Get("stub")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []bool
	agent.Register(scripted{"stub", func(task agent.Task) agent.Result {
		mu.Lock()
		seen = append(seen, task.Gate)
		mu.Unlock()
		return agent.Result{Status: agent.StatusOK, Final: "done"}
	}})
	t.Cleanup(func() { agent.Register(orig) })
	return func() []bool {
		mu.Lock()
		defer mu.Unlock()
		return append([]bool(nil), seen...)
	}
}

func TestMCPRunPassesCommandGateToTheTask(t *testing.T) {
	seen := recordGate(t)
	s := connect(t, t.TempDir())
	if r := s.call(t, "run", `{"prompt":"x","backend":"stub","command_gate":true}`); r.IsError {
		t.Fatalf("run: %s", r.Text())
	}
	if got := seen(); len(got) != 1 || !got[0] {
		t.Fatalf("the task's gate = %v, want on", got)
	}
}

func TestMCPBatchKeepsCommandGateApartFromItsGateCommands(t *testing.T) {
	seen := recordGate(t)
	gate := installFakeGate(t)
	s := connect(t, gitRepo(t))
	r := s.call(t, "batch", `{"spec":{"backend":"stub","command_gate":true,"gate":["git no-such-subcommand"],"tasks":[{"id":"one","prompt":"x"}]}}`)
	if !strings.Contains(r.Text(), "no-such-subcommand") {
		t.Fatalf("batch should fail on its gate command: %s", r.Text())
	}
	if got := seen(); len(got) != 1 || !got[0] {
		t.Fatalf("the task's gate = %v, want on", got)
	}
	// With the option on, the gate command itself runs through the gate.
	if ran, err := os.ReadFile(filepath.Join(gate, "ran")); err != nil || string(ran) != "git no-such-subcommand" {
		t.Fatalf("the gate ran %q (%v), want the gate command", ran, err)
	}
}
