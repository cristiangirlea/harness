package tools

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/envpolicy"
	"github.com/cristiangirlea/harness/internal/gitenv"
	"github.com/cristiangirlea/harness/internal/nowindow"
)

const (
	// GateName is the gate binary hrn looks for in PATH.
	GateName = "adanext-exec"
	// GateProtocol is the gate's JSON protocol version hrn speaks.
	GateProtocol = "adanext-exec/1"
)

// The gate kills the command's tree at the timeout; hrn kills the gate only if it overstays this.
var gateGrace = 15 * time.Second

// The gate's own exit codes for a command it did not run (docs/protocol.md in adanext-exec).
const gateRefused = 125

var gateNotRun = []int{121, 123, gateRefused, 126, 127}

const gateTimedOut = 124

const gatePolicyPrefix = "hrn-gate-policy-"

// gatePolicy is the run's guard in the gate's policy format. Every built-in is named so that a gate
// lacking one rejects the policy at the handshake, rather than silently allowing what hrn refuses.
func gatePolicy(allowDangerous bool) ([]byte, error) {
	type rule struct {
		Name     string `json:"name"`
		Match    string `json:"match,omitempty"`
		Decision string `json:"decision"`
	}
	decision := "refuse"
	if allowDangerous {
		decision = "allow"
	}
	var rules []rule
	for _, r := range guardRules {
		if r.name != "guard-env" {
			rules = append(rules, rule{Name: r.name, Decision: decision})
		}
	}
	if !allowDangerous {
		// Both guard what the gate cannot know is hrn's: its run environment and this policy.
		rules = append(rules, rule{Name: "guard-env", Match: guardEnvPattern(), Decision: "refuse"},
			rule{Name: "gate-policy-file", Match: `(?i)` + regexp.QuoteMeta(gatePolicyPrefix), Decision: "refuse"})
	}
	return json.Marshal(map[string]any{"version": 1, "default": "allow", "rules": rules})
}

// WriteGatePolicy writes the policy hrn hands the gate for one run and returns its path and how to remove it.
func WriteGatePolicy(allowDangerous bool) (string, func(), error) {
	data, err := gatePolicy(allowDangerous)
	if err != nil {
		return "", nil, fmt.Errorf("gate: %w", err)
	}
	f, err := os.CreateTemp("", gatePolicyPrefix+"*.json")
	if err != nil {
		return "", nil, fmt.Errorf("gate: %w", err)
	}
	done := func() { _ = os.Remove(f.Name()) }
	_, err = f.Write(data)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	// Commands run in the workspace, so a relative temp directory would name another file there.
	path, absErr := filepath.Abs(f.Name())
	if err == nil {
		err = absErr
	}
	if err != nil {
		done()
		return "", nil, fmt.Errorf("gate: %w", err)
	}
	return path, done, nil
}

type gateAnswer struct {
	Version, Decision, Rule, Error string
}

// askGate is the gate's check of one request; reason is the first thing it said, for an error.
func askGate(ctx context.Context, gate, policy string, env []string, request []byte) (answer gateAnswer, reason string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{"check", "--json"}
	if policy != "" {
		args = append(args, "--policy", policy)
	}
	cmd := exec.CommandContext(ctx, gate, args...)
	nowindow.Set(cmd)
	cmd.Env = envpolicy.Filter(gitenv.WithoutRepoVars(env))
	cmd.Stdin = bytes.NewReader(request)
	stdout := &boundedBuffer{keepHead: 16 << 10, keepTail: 1 << 10}
	stderr := &boundedBuffer{keepHead: 16 << 10, keepTail: 1 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 5 * time.Second
	err = cmd.Run()
	_ = json.Unmarshal([]byte(stdout.String()), &answer)
	return answer, firstLine(answer.Error, stderr.String(), stdout.String()), err
}

// guardRule is the rule refusing a command: the gate's answer once hrn has handed it the guard,
// else hrn's own table. An unusable answer refuses nothing here, since the gate's run judges again.
func (ws *Workspace) guardRule(command string) string {
	if ws.AllowDangerous {
		return ""
	}
	if ws.GatePolicy == "" {
		return Guard(command)
	}
	shell, _ := Shell()
	request, err := json.Marshal(map[string]string{"version": GateProtocol, "shell": shell, "script": command})
	if err != nil {
		return ""
	}
	ctx := ws.RunContext
	if ctx == nil {
		ctx = context.Background()
	}
	answer, _, err := askGate(ctx, ws.Gate, ws.GatePolicy, append(os.Environ(), ws.ChildEnv...), request)
	if err != nil || answer.Version != GateProtocol || answer.Decision == "" || answer.Decision == "allow" {
		return ""
	}
	return cmp.Or(answer.Rule, gateNoRule)
}

// FindGate returns the gate's path once it answers the protocol handshake under the run's policy,
// so a run with the gate on never starts without one or with a policy it cannot use.
func FindGate(ctx context.Context, env []string, policy string) (string, error) {
	path, err := exec.LookPath(GateName)
	if err != nil {
		return "", fmt.Errorf("gate: %s is not in PATH (%v); install it, or run without --gate", GateName, err)
	}
	if ext := strings.ToLower(filepath.Ext(path)); ext == ".bat" || ext == ".cmd" {
		return "", fmt.Errorf("gate: %s is a batch file, whose arguments cmd.exe parses again; install the %s program itself", path, GateName)
	}
	answer, reason, runErr := askGate(ctx, path, policy, env, []byte(`{"version":"`+GateProtocol+`","args":["git","status"]}`))
	if runErr == nil && answer.Version == GateProtocol &&
		slices.Contains([]string{"allow", "ask", "refuse"}, answer.Decision) {
		return path, nil
	}
	if answer.Version != "" && answer.Version != GateProtocol {
		reason = "it speaks " + answer.Version
	}
	if runErr != nil {
		reason = runErr.Error() + ": " + reason
	}
	return "", fmt.Errorf("gate: %s failed the %s handshake (%s); it may be older than this hrn, or its policy cannot be used",
		path, GateProtocol, reason)
}

// firstLine is the first non-empty line among texts, for a short reason.
func firstLine(texts ...string) string {
	for _, text := range texts {
		for line := range strings.SplitSeq(text, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				return line
			}
		}
	}
	return "no answer"
}

// runGated runs a command through the gate, which applies its policy, kills the command's tree
// at the timeout and writes the audit line; a gate that cannot run the command is an error.
func runGated(ctx context.Context, ws *Workspace, command string, timeout time.Duration, env []string) (ShellResult, error) {
	// As the gate's own hook does: a nested run with these flags could approve its own ask.
	if strings.Contains(command, "-approved") || strings.Contains(command, "-script-b64") {
		return ShellResult{}, fmt.Errorf(hrnRefused+"a command that names %s's -approved or -script-b64 flag could approve its own ask, and only a person approves one", GateName)
	}
	return gateRun(ctx, ws, command, timeout, env)
}

// RunGated runs an operator's command, such as a batch gate, in dir through the gate at path under
// policy, auditing it under runDir's run id; an operator's own command is not checked for approval flags.
func RunGated(ctx context.Context, path, policy, dir, runDir, command string, timeout time.Duration, env []string) (ShellResult, error) {
	res, err := gateRun(ctx, &Workspace{Root: dir, Gate: path, GatePolicy: policy, RunDir: runDir}, command, timeout, env)
	if r, ok := errors.AsType[*gateRefusal](err); ok {
		// The advice in a refusal is the model's; an operator reads the gate's own reason.
		err = errors.New(gateRefusedBy + r.said)
	}
	return res, err
}

// gateRefusal is the gate's refusal of a command it judged; said is the gate's message.
type gateRefusal struct{ said string }

func (r *gateRefusal) Error() string {
	return gateRefusedBy + r.said + ". Do not retry it unchanged; say in your answer why it is needed"
}

func gateRun(ctx context.Context, ws *Workspace, command string, timeout time.Duration, env []string) (ShellResult, error) {
	shell, _ := Shell()
	argv := []string{ws.Gate, "run", "--shell", shell, "--agent", "hrn"}
	if ws.GatePolicy != "" {
		argv = append(argv, "--policy", ws.GatePolicy)
	}
	backstop := time.Duration(0)
	if timeout > 0 {
		argv = append(argv, "--timeout", timeout.String())
		backstop = timeout + gateGrace
	}
	if ws.RunDir != "" {
		argv = append(argv, "--run-id", filepath.Base(ws.RunDir))
	}
	argv = append(argv, "--", command)
	started := time.Now()
	res, err := runArgv(ctx, ws.Root, argv, backstop, env, ws.ShapeTools, startGateTree)
	if err != nil {
		if errors.Is(err, ErrWait) {
			return res, err
		}
		return res, fmt.Errorf(gateCouldNot+", so the command did not run: %w", err)
	}
	if res.TimedOut || ctx.Err() != nil {
		return res, nil
	}
	// The gate's codes can also be the command's own: a command the gate did not run has only
	// the gate's message as output, and its timeout comes no sooner than the one hrn gave it.
	said := strings.HasPrefix(res.Output, GateName)
	switch {
	case res.ExitCode == gateTimedOut && timeout > 0 && time.Since(started) >= timeout:
		res.TimedOut, res.ExitCode = true, -1
	case res.ExitCode == gateRefused && said:
		return res, &gateRefusal{strings.TrimSpace(res.Output)}
	case slices.Contains(gateNotRun, res.ExitCode) && said:
		return res, fmt.Errorf(gateDidNotRun+" (exit %d): %s; nothing ran in its place", res.ExitCode, strings.TrimSpace(res.Output))
	}
	return res, nil
}
