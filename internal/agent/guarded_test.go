package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/cristiangirlea/harness/internal/tools"
)

// counting is a backend that only counts its runs.
type counting struct{ runs *int }

func (c counting) Name() string            { return "counting" }
func (c counting) Detect() (string, error) { return "built-in", nil }
func (c counting) Run(context.Context, Task, func(Event)) Result {
	*c.runs++
	return Result{Backend: "counting", Status: StatusOK}
}

// An hrn started inside a guarded run, from an agent's shell or as an MCP
// server its CLI started, does not lift the guard, whoever asks it to.
func TestAGuardedRunCannotLiftTheGuard(t *testing.T) {
	t.Setenv(tools.GuardedVar, "1")
	if err := ValidateOptions(Options{AllowDangerous: true}); !errors.Is(err, ErrGuardedRun) {
		t.Errorf("Validate = %v, want ErrGuardedRun", err)
	}
	if err := ValidateOptions(Options{Mode: ModeYolo}); err != nil {
		t.Errorf("a guarded hrn's own guarded run: %v", err)
	}
	// Run holds it for an entry point that does not validate.
	runs := 0
	res := Run(context.Background(), counting{&runs}, Task{Prompt: "p", Options: Options{AllowDangerous: true}}, func(Event) {})
	if runs != 0 || res.Status != StatusRefused {
		t.Errorf("runs = %d, status %s; want refused before the backend runs", runs, res.Status)
	}
	if res = Run(context.Background(), counting{&runs}, Task{Prompt: "p"}, func(Event) {}); runs != 1 || res.Status != StatusOK {
		t.Errorf("runs = %d, status %s; a guarded run stays allowed", runs, res.Status)
	}

	t.Setenv(tools.GuardedVar, "")
	if err := ValidateOptions(Options{AllowDangerous: true}); err != nil {
		t.Errorf("the operator's hrn: %v", err)
	}
}

// Nor does it hand the agent's runs the operator's whole environment: only the operator may.
func TestAGuardedRunCannotInheritTheEnvironment(t *testing.T) {
	t.Setenv(tools.GuardedVar, "1")
	if err := ValidateOptions(Options{InheritEnv: true}); !errors.Is(err, ErrGuardedInherit) {
		t.Errorf("Validate = %v, want ErrGuardedInherit", err)
	}
	runs := 0
	res := Run(context.Background(), counting{&runs}, Task{Prompt: "p", Options: Options{InheritEnv: true}}, func(Event) {})
	if runs != 0 || res.Status != StatusRefused {
		t.Errorf("runs = %d, status %s; want refused before the backend runs", runs, res.Status)
	}
	t.Setenv(tools.GuardedVar, "")
	if err := ValidateOptions(Options{InheritEnv: true}); err != nil {
		t.Errorf("the operator's hrn: %v", err)
	}
}

func TestValidateOptionsGuardOrder(t *testing.T) {
	t.Setenv(tools.GuardedVar, "1")
	for _, tc := range []struct {
		o    Options
		want string
	}{
		{Options{Retrieval: "bad", AllowDangerous: true}, "invalid retrieval mode"},
		{Options{AllowDangerous: true, MaxCostUSD: -1}, ErrGuardedRun.Error()},
		{Options{InheritEnv: true, Timeout: -1}, ErrGuardedInherit.Error()},
	} {
		if err := ValidateOptions(tc.o); err == nil || err.Error() != tc.want {
			t.Errorf("ValidateOptions(%+v) = %v, want %s", tc.o, err, tc.want)
		}
	}
}
