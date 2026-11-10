package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/tools"
)

func gateRunArgs(t *testing.T, gateDir string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(gateDir, "runs.log"))
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	if err := json.Unmarshal(data, &args); err != nil {
		t.Fatal(err)
	}
	return args
}

func TestAPIRunHandsTheGateTheGuardAsAPolicyAndRemovesIt(t *testing.T) {
	gateDir := installFakeGate(t)
	res, _, _ := runFake(t, [][]string{
		toolTurn("toolu_c", "run_command", `{"command":"echo sudo"}`),
		textTurn("done"),
	}, Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Mode: ModeYolo, Gate: true}})
	if res.Status != StatusOK {
		t.Fatalf("result = %s %q", res.Status, res.Error)
	}
	policy, ok := argAfter(gateRunArgs(t, gateDir), "--policy")
	if !ok {
		t.Fatal("the gate ran the command with no policy from hrn")
	}
	seen, err := os.ReadFile(filepath.Join(gateDir, "policy.seen"))
	if err != nil || !strings.Contains(string(seen), `"guard-env"`) {
		t.Fatalf("the gate saw policy %s (%v), want the guard's", seen, err)
	}
	if _, err := os.Stat(policy); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the policy outlived the run: %v", err)
	}
}

func TestAPIRunWithAllowDangerousHandsTheGateTheLiftedPolicy(t *testing.T) {
	gateDir := installFakeGate(t)
	res, _, _ := runFake(t, [][]string{
		toolTurn("toolu_c", "run_command", `{"command":"echo hi"}`),
		textTurn("done"),
	}, Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Mode: ModeYolo, Gate: true, AllowDangerous: true}})
	if res.Status != StatusOK {
		t.Fatalf("result = %s %q", res.Status, res.Error)
	}
	seen, err := os.ReadFile(filepath.Join(gateDir, "policy.seen"))
	if err != nil || !strings.Contains(string(seen), `"git-push"`) || strings.Contains(string(seen), `"refuse"`) {
		t.Fatalf("the gate saw policy %s (%v), want every rule lifted", seen, err)
	}
}

func scopeTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	return dir
}

func assertNoPolicyLeft(t *testing.T, dir string) {
	t.Helper()
	left, err := filepath.Glob(filepath.Join(dir, "hrn-gate-policy-*"))
	if err != nil || len(left) != 0 {
		t.Fatalf("policy files left behind: %q (%v)", left, err)
	}
}

func TestARunThatCannotFindTheGateLeavesNoPolicyFile(t *testing.T) {
	dir := scopeTemp(t)
	t.Setenv("PATH", t.TempDir())
	res, _, _ := runFake(t, [][]string{textTurn("done")}, Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Mode: ModeYolo, Gate: true}})
	if res.Status != StatusError {
		t.Fatalf("result = %s %q", res.Status, res.Error)
	}
	assertNoPolicyLeft(t, dir)
}

func TestAClaudeRunRefusedOutsideYoloLeavesNoPolicyFile(t *testing.T) {
	dir := scopeTemp(t)
	installFakeGate(t)
	res, _, rec := runGatedClaude(t, "routed", Options{Mode: ModeAuto, Gate: true})
	if res.Status != StatusError || rec != nil {
		t.Fatalf("result = %s %q, started %v", res.Status, res.Error, rec != nil)
	}
	assertNoPolicyLeft(t, dir)
}

func TestAPIRunReportsTheGatesRefusalAsTheGuardsAndAsksOnce(t *testing.T) {
	gateDir := installFakeGate(t)
	res, _, fake := runFake(t, [][]string{
		toolTurn("toolu_c", "run_command", `{"command":"fake-refuse now"}`),
		textTurn("done"),
	}, Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Mode: ModeYolo, Gate: true}})
	if res.Status != StatusOK {
		t.Fatalf("result = %s %q", res.Status, res.Error)
	}
	block := lastToolResult(t, fake.requests[1])
	if block["is_error"] != true || fmt.Sprint(block["content"]) != tools.GuardError("git-push").Error() {
		t.Fatalf("tool result = %v, want the guard's refusal naming the gate's rule", block)
	}
	if _, err := os.Stat(filepath.Join(gateDir, "runs.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused command reached run: %v", err)
	}
	checks, err := os.ReadFile(filepath.Join(gateDir, "checks.log"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(checks), "\n"); got != 2 {
		t.Fatalf("the gate was asked %d times, want the handshake and one check:\n%s", got, checks)
	}
}

func TestGatedClaudeHandsTheHookThePolicyAndRemovesIt(t *testing.T) {
	gateDir := installFakeGate(t)
	res, _, rec := runGatedClaude(t, "routed", Options{Mode: ModeYolo, Gate: true})
	if res.Status != StatusOK || rec == nil {
		t.Fatalf("result = %s %q", res.Status, res.Error)
	}
	data, err := os.ReadFile(filepath.Join(gateDir, "hook.args"))
	if err != nil {
		t.Fatal(err)
	}
	var probe []string
	if err := json.Unmarshal(data, &probe); err != nil {
		t.Fatal(err)
	}
	policy, ok := argAfter(probe, "--policy")
	if !ok {
		t.Fatalf("the hook's start-up probe has no policy: %q", probe)
	}
	if !strings.Contains(rec.Settings, "hook claude-code --policy ") || !strings.Contains(rec.Settings, filepath.Base(policy)) {
		t.Errorf("settings = %s, want the hook started with the run's policy", rec.Settings)
	}
	if _, err := os.Stat(policy); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the policy outlived the run: %v", err)
	}
}

// realGateHook asks the real binary's hook from an empty home, so no Claude Code settings count.
func realGateHook(t *testing.T, policy, command string) gateAnswerRead {
	t.Helper()
	gate := os.Getenv("HRN_TEST_GATE")
	if gate == "" {
		t.Skip("HRN_TEST_GATE names no real adanext-exec")
	}
	home := t.TempDir()
	cmd := exec.CommandContext(context.Background(), gate, "hook", "claude-code", "--policy", policy)
	cmd.Dir = home
	cmd.Env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home, "CLAUDE_PROJECT_DIR="+home)
	input, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash",
		"tool_input": map[string]any{"command": command, "timeout": 1000}})
	cmd.Stdin = strings.NewReader(string(input))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	a := readGateAnswer("Bash", string(out), gate, policy)
	if !a.denied && (!a.answered || !a.timed) {
		t.Fatalf("hook answer for %q = %s (%+v), want one hrn reads as the gate's", command, out, a)
	}
	return a
}

func TestRealGateHookJudgesByTheRunsPolicyAndHrnReadsItsRewrite(t *testing.T) {
	refusing, done, err := tools.WriteGatePolicy(false)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	lifted, doneLifted, err := tools.WriteGatePolicy(true)
	if err != nil {
		t.Fatal(err)
	}
	defer doneLifted()
	if a := realGateHook(t, refusing, "git push origin main"); !a.denied {
		t.Errorf("normal policy: answer = %+v, want a refusal", a)
	}
	if a := realGateHook(t, refusing, "export GH_TOKEN=x"); !a.denied {
		t.Errorf("normal policy: guard-env answer = %+v, want a refusal", a)
	}
	if a := realGateHook(t, lifted, "git push origin main"); a.denied || a.command != "git push origin main" {
		t.Errorf("lifted policy: answer = %+v, want the command allowed and read back", a)
	}
}
