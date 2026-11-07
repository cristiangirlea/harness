package hrnmcp

import (
	"strings"
	"sync"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
)

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
	s := connect(t, gitRepo(t))
	r := s.call(t, "batch", `{"spec":{"backend":"stub","command_gate":true,"gate":["git no-such-subcommand"],"tasks":[{"id":"one","prompt":"x"}]}}`)
	if !strings.Contains(r.Text(), "no-such-subcommand") {
		t.Fatalf("batch should fail on its gate command: %s", r.Text())
	}
	if got := seen(); len(got) != 1 || !got[0] {
		t.Fatalf("the task's gate = %v, want on", got)
	}
}
