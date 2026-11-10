package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/tools"
)

func TestGateToolsNeedsTheGateThePolicyAndItsSum(t *testing.T) {
	for _, args := range [][]string{nil, {"--gate", "g"}, {"--policy", "p"}, {"--gate", "g", "--policy", "p"}} {
		if err := cmdGateTools(args); err == nil || !strings.Contains(err.Error(), "are required") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
}

func TestGateToolsPassesEveryFlagToTheWorkspace(t *testing.T) {
	dir := t.TempDir()
	policy := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, err := tools.GatePolicySum(policy)
	if err != nil {
		t.Fatal(err)
	}
	ws, got, err := gateToolsWorkspace([]string{"--dir", dir, "--gate", "the-gate", "--policy", policy,
		"--policy-sha256", sum, "--run-dir", "RUN", "--allow-dangerous", "--shape-tools"})
	if err != nil {
		t.Fatal(err)
	}
	if ws.Gate != "the-gate" || ws.GatePolicy != policy || got != sum || ws.RunDir != "RUN" || !ws.AllowDangerous || !ws.ShapeTools {
		t.Fatalf("workspace = gate %q policy %q sum %q run %q dangerous %v shape %v", ws.Gate, ws.GatePolicy, got, ws.RunDir, ws.AllowDangerous, ws.ShapeTools)
	}
}

func TestGateToolsRefusesAPolicyThatNoLongerMatchesItsSum(t *testing.T) {
	policy := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policy, []byte(`{"version":1,"default":"allow"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := gateToolsWorkspace([]string{"--gate", "g", "--policy", policy, "--policy-sha256", strings.Repeat("0", 64)})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v", err)
	}
}
