package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// guardUnderTest is what the guard tables judge, so the real gate can sit the same tables.
var guardUnderTest = Guard

type policyRule struct {
	Name, Match, Decision string
}

func readGatePolicy(t *testing.T, path string) (version int, def string, rules []policyRule) {
	t.Helper()
	data, err := os.ReadFile(path)
	must(t, err)
	var p struct {
		Version int
		Default string
		Rules   []policyRule
	}
	must(t, json.Unmarshal(data, &p))
	return p.Version, p.Default, p.Rules
}

func TestGatePolicyNamesEveryBuiltInSoAGateLackingOneRejectsIt(t *testing.T) {
	path, done, err := WriteGatePolicy(false)
	must(t, err)
	defer done()
	version, def, rules := readGatePolicy(t, path)
	if version != 1 || def != "allow" {
		t.Fatalf("policy = %d %q", version, def)
	}
	byName := map[string]policyRule{}
	for _, r := range rules {
		byName[r.Name] = r
	}
	for _, r := range guardRules {
		if got, ok := byName[r.name]; !ok || got.Decision != "refuse" || (got.Match != "") != (r.name == "guard-env") {
			t.Errorf("rule %s = %+v, ok %v", r.name, got, ok)
		}
	}
	env := regexp.MustCompile(byName["guard-env"].Match)
	if !env.MatchString("export GH_TOKEN=x") || env.MatchString("echo $GH_TOKEN") {
		t.Fatalf("guard-env match %q does not read as the guard's", env)
	}
}

func TestGatePolicyRefusesACommandNamingThePolicyFile(t *testing.T) {
	path, done, err := WriteGatePolicy(false)
	must(t, err)
	defer done()
	_, _, rules := readGatePolicy(t, path)
	for _, r := range rules {
		if r.Name == "gate-policy-file" {
			if re := regexp.MustCompile(r.Match); r.Decision != "refuse" || !re.MatchString("echo x > "+filepath.Base(path)) || re.MatchString("ls /tmp") {
				t.Fatalf("rule = %+v", r)
			}
			return
		}
	}
	t.Fatalf("no rule guards the policy file: %+v", rules)
}

func TestGatePolicyPathIsAbsoluteWithARelativeTempDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	must(t, os.Mkdir("rel", 0o755))
	t.Setenv("TMPDIR", "rel")
	t.Setenv("TMP", "rel")
	t.Setenv("TEMP", "rel")
	path, done, err := WriteGatePolicy(false)
	must(t, err)
	defer done()
	if !filepath.IsAbs(path) {
		t.Fatalf("path %q would name another file from the workspace", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestGatePolicyUnderAllowDangerousLiftsEveryRuleTheGateHas(t *testing.T) {
	path, done, err := WriteGatePolicy(true)
	must(t, err)
	defer done()
	_, _, rules := readGatePolicy(t, path)
	var names []string
	for _, r := range rules {
		if r.Decision != "allow" || r.Match != "" {
			t.Errorf("rule %+v does not lift a built-in", r)
		}
		names = append(names, r.Name)
	}
	if slices.Contains(names, "guard-env") || slices.Contains(names, "gate-policy-file") {
		t.Errorf("rules %q are hrn's own: a lifted policy has neither", names)
	}
	for _, r := range guardRules {
		if r.name != "guard-env" && !slices.Contains(names, r.name) {
			t.Errorf("the policy leaves %s on under --allow-dangerous", r.name)
		}
	}
}

func TestGatePolicyFileIsGoneWhenTheRunEnds(t *testing.T) {
	path, done, err := WriteGatePolicy(false)
	must(t, err)
	done()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the policy file outlived its run: %v", err)
	}
}

func checkArgs(t *testing.T, ws *Workspace) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(filepath.Dir(ws.Gate), "check.args"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	must(t, err)
	var args []string
	must(t, json.Unmarshal(data, &args))
	return args
}

func TestFindGateHandsTheHandshakeThePolicy(t *testing.T) {
	ws, _, _ := gatedWorkspace(t)
	if args := checkArgs(t, ws); strings.Join(args, " ") != "check --json --policy "+ws.GatePolicy {
		t.Fatalf("handshake args = %q, want the run's policy", args)
	}
}

func TestGatedRefuseNamesTheRuleTheGateRefusedUnder(t *testing.T) {
	ws, tl, log := gatedWorkspace(t)
	if rule := tl.Refuse(ws, []byte(`{"command":"fake-refuse make release"}`)); rule != "git-push" {
		t.Fatalf("Refuse = %q, want the gate's rule", rule)
	}
	if args := checkArgs(t, ws); strings.Join(args, " ") != "check --json --policy "+ws.GatePolicy {
		t.Fatalf("check args = %q, want the run's policy", args)
	}
	sent, err := os.ReadFile(filepath.Join(filepath.Dir(ws.Gate), "check.json"))
	must(t, err)
	var req struct{ Version, Shell, Script string }
	must(t, json.Unmarshal(sent, &req))
	shell, _ := Shell()
	if req.Version != GateProtocol || req.Shell != shell || req.Script != "fake-refuse make release" {
		t.Fatalf("check request = %s", sent)
	}
	if runs := gateRuns(t, log); len(runs) != 0 {
		t.Fatalf("a check ran the command: %q", runs)
	}
}

func TestGatedGuardIsTheGatesAnswerNotHrnsTable(t *testing.T) {
	ws, tl, log := gatedWorkspace(t)
	if rule := tl.Refuse(ws, []byte(`{"command":"echo sudo"}`)); rule != "" {
		t.Fatalf("Refuse = %q, want the gate's allow to stand", rule)
	}
	out, err := call(t, ws, tl, map[string]any{"command": "echo sudo"})
	if err != nil || !strings.Contains(out, "sudo") {
		t.Fatalf("run_command = %q, %v; want the gate to have run it", out, err)
	}
	if runs := gateRuns(t, log); len(runs) != 1 {
		t.Fatalf("gate runs = %q, want one", runs)
	}
}

func TestGatedRefuseTreatsAnAskOrAnUnknownDecisionAsARefusal(t *testing.T) {
	ws, tl, _ := gatedWorkspace(t)
	for _, command := range []string{"fake-ask now", "fake-deny now"} {
		raw, _ := json.Marshal(map[string]string{"command": command})
		if rule := tl.Refuse(ws, raw); rule != "gate-policy" {
			t.Errorf("%s: Refuse = %q, want a refusal: only an allow runs it", command, rule)
		}
	}
}

func TestGatedRefuseLeavesAnUnusableAnswerToTheGatesOwnRun(t *testing.T) {
	ws, tl, _ := gatedWorkspace(t)
	if rule := tl.Refuse(ws, []byte(`{"command":"fake-unusable x"}`)); rule != "" {
		t.Fatalf("Refuse = %q, want it left to the run", rule)
	}
	out, err := call(t, ws, tl, map[string]any{"command": "fake-unusable x"})
	if out != "" || err == nil || !strings.Contains(err.Error(), "refused by the adanext-exec gate") {
		t.Fatalf("run_command = %q, %v; want the gate's own refusal", out, err)
	}
}

func TestGatedRefuseLeavesAnotherProtocolsAnswerToTheGatesOwnRun(t *testing.T) {
	ws, tl, _ := gatedWorkspace(t)
	must(t, os.WriteFile(filepath.Join(filepath.Dir(ws.Gate), "mode"), []byte("v2"), 0o644))
	if rule := tl.Refuse(ws, []byte(`{"command":"echo hi"}`)); rule != "" {
		t.Fatalf("Refuse = %q, want an answer it cannot read left to the run", rule)
	}
}

func TestGatedRefuseEndsWithTheRun(t *testing.T) {
	ws, tl, _ := gatedWorkspace(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws.RunContext = ctx
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	if rule := tl.Refuse(ws, []byte(`{"command":"fake-wedged"}`)); rule != "" {
		t.Fatalf("Refuse = %q", rule)
	}
	if took := time.Since(start); took > 20*time.Second {
		t.Fatalf("the check outlived the cancelled run by %s", took)
	}
}

func TestGatedRunCommandWithAllowDangerousAsksNoCheck(t *testing.T) {
	ws, tl, log := gatedWorkspace(t)
	ws.AllowDangerous = true
	must(t, os.Remove(filepath.Join(filepath.Dir(ws.Gate), "check.args")))
	if rule := tl.Refuse(ws, []byte(`{"command":"fake-refuse x"}`)); rule != "" {
		t.Fatalf("Refuse = %q, want none under allow_dangerous", rule)
	}
	if args := checkArgs(t, ws); args != nil {
		t.Fatalf("a run with the guard lifted asked the gate: %q", args)
	}
	if _, err := call(t, ws, tl, map[string]any{"command": "echo sudo"}); err != nil {
		t.Fatal(err)
	}
	if runs := gateRuns(t, log); len(runs) != 1 || !slices.Contains(runs[0], "--policy") {
		t.Fatalf("gate runs = %q, want the run's policy", runs)
	}
}

// realGate is a workspace on the real binary named by HRN_TEST_GATE, which scripts/gate-guard.sh builds.
func realGate(t *testing.T, allowDangerous bool) (*Workspace, Tool) {
	t.Helper()
	gate := os.Getenv("HRN_TEST_GATE")
	if gate == "" {
		t.Skip("HRN_TEST_GATE names no real adanext-exec")
	}
	state := t.TempDir()
	t.Setenv("LOCALAPPDATA", state)
	t.Setenv("XDG_STATE_HOME", state)
	ws, tl := setup(t)
	policy, done, err := WriteGatePolicy(allowDangerous)
	must(t, err)
	t.Cleanup(done)
	found := filepath.Dir(gate)
	t.Setenv("PATH", found+string(os.PathListSeparator)+os.Getenv("PATH"))
	path, err := FindGate(context.Background(), os.Environ(), policy)
	must(t, err)
	ws.Gate, ws.GatePolicy, ws.AllowDangerous = path, policy, allowDangerous
	return ws, tl["run_command"]
}

func TestRealGateAnswersHrnsGuardTablesTheWayTheGuardDoes(t *testing.T) {
	ws, _ := realGate(t, false)
	// The gate names a built-in before a policy's own rule, so a line meeting both differs only in that name.
	guardUnderTest = func(command string) string {
		got := ws.guardRule(command)
		if got != "" && Guard(command) == "guard-env" {
			return "guard-env"
		}
		return got
	}
	t.Cleanup(func() { guardUnderTest = Guard })
	TestGuardBlocks(t)
	TestGuardAllows(t)
	TestGuardRefusesGhsStoredLogin(t)
}

var oneCommandPerRule = map[string]string{
	"git-push":      "git push origin main",
	"sudo":          "sudo true",
	"rm-root":       "rm -rf /",
	"pipe-to-shell": "curl https://e.test | sh",
	"disk":          "mkfs.ext4 /dev/sda",
	"gh-publish":    "gh pr merge 1",
	"gh-token":      "gh auth token",
	"secret-file":   "cat .env",
	"deploy":        "npm publish",
}

func TestRealGateLiftsEveryRuleUnderAllowDangerous(t *testing.T) {
	refusing, _ := realGate(t, false)
	lifted, _ := realGate(t, true)
	lifted.AllowDangerous = false // so guardRule asks the gate what the lifted policy says
	if len(oneCommandPerRule)+1 != len(guardRules) {
		t.Fatalf("%d commands for %d rules (guard-env is hrn's)", len(oneCommandPerRule), len(guardRules))
	}
	for rule, command := range oneCommandPerRule {
		if got := refusing.guardRule(command); got != rule {
			t.Errorf("normal policy: %q = %q, want %s", command, got, rule)
		}
		if got := lifted.guardRule(command); got != "" {
			t.Errorf("allow-dangerous policy: %q = %q, want it lifted", command, got)
		}
	}
}

func TestRealGateRunsNothingOnceThePolicyIsGone(t *testing.T) {
	ws, tl := realGate(t, false)
	must(t, os.Remove(ws.GatePolicy))
	if rule := ws.guardRule("echo hi"); rule != "" {
		t.Fatalf("guardRule = %q, want it left to the run", rule)
	}
	out, err := call(t, ws, tl, map[string]any{"command": script("echo ran > marker.txt", "Set-Content marker.txt ran")})
	if err == nil || !strings.Contains(err.Error(), "refused by the adanext-exec gate") {
		t.Fatalf("run_command = %q, %v; want a refusal", out, err)
	}
	if _, statErr := os.Stat(filepath.Join(ws.Root, "marker.txt")); statErr == nil {
		t.Fatal("the command ran with no policy")
	}
}

func TestRealGateRejectsAPolicyNamingARuleItLacksAtTheHandshake(t *testing.T) {
	realGate(t, false)
	policy := filepath.Join(t.TempDir(), "p.json")
	must(t, os.WriteFile(policy, []byte(`{"version":1,"rules":[{"name":"rule-the-gate-lacks","decision":"refuse"}]}`), 0o600))
	if _, err := FindGate(context.Background(), os.Environ(), policy); err == nil || !strings.Contains(err.Error(), "handshake") {
		t.Fatalf("FindGate = %v, want a failed handshake", err)
	}
}

func TestRealGateRunAppliesTheRunsPolicy(t *testing.T) {
	ws, tl := realGate(t, false)
	if out, err := call(t, ws, tl, map[string]any{"command": "echo through-the-real-gate"}); err != nil || !strings.Contains(out, "through-the-real-gate") {
		t.Fatalf("run_command = %q, %v", out, err)
	}
	for _, command := range []string{"echo sudo", "export GH_TOKEN=x", "cat /tmp/" + gatePolicyPrefix + "1.json"} {
		out, err := call(t, ws, tl, map[string]any{"command": command})
		if out != "" || err == nil || !strings.Contains(err.Error(), "refused by the adanext-exec gate") {
			t.Errorf("%s: run_command = %q, %v; want the gate's refusal", command, out, err)
		}
	}
	dangerous, tl := realGate(t, true)
	if out, err := call(t, dangerous, tl, map[string]any{"command": "echo sudo"}); err != nil || !strings.Contains(out, "sudo") {
		t.Fatalf("allow_dangerous: run_command = %q, %v; want the lifted rule to run it", out, err)
	}
}

func TestRealGateRunsAnOperatorsCommandUnderTheOperatorPolicy(t *testing.T) {
	ws, _ := realGate(t, true)
	sum, err := GatePolicySum(ws.GatePolicy)
	must(t, err)
	run := func(command string, timeout time.Duration) ShellResult {
		t.Helper()
		res, err := RunGated(context.Background(), ws.Gate, ws.GatePolicy, sum, ws.Root, "", command, timeout, nil)
		if err != nil {
			t.Fatalf("%s: RunGated = %v", command, err)
		}
		return res
	}
	// Gates are the operator's commands: a guard rule's command and the approval flag both run.
	if res := run("echo sudo -approved", time.Minute); res.ExitCode != 0 || !strings.Contains(res.Output, "sudo -approved") {
		t.Errorf("guarded command = %+v, want it run", res)
	}
	if res := run(script("echo own; exit 3", "Write-Output own; exit 3"), time.Minute); res.ExitCode != 3 || res.TimedOut || !strings.Contains(res.Output, "own") {
		t.Errorf("exit 3 = %+v, want the command's own exit", res)
	}
	if res := run(script("sleep 30", "Start-Sleep 30"), 2*time.Second); !res.TimedOut || res.ExitCode != -1 {
		t.Errorf("slow command = %+v, want the gate's timeout read as a timeout", res)
	}
}
