package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/envpolicy"
	"github.com/cristiangirlea/harness/internal/gitenv"
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

// FindGate returns the gate's path once it answers the protocol handshake, so a run with the
// gate on never starts without one.
func FindGate(ctx context.Context, env []string) (string, error) {
	path, err := exec.LookPath(GateName)
	if err != nil {
		return "", fmt.Errorf("gate: %s is not in PATH (%v); install it, or run without --gate", GateName, err)
	}
	if ext := strings.ToLower(filepath.Ext(path)); ext == ".bat" || ext == ".cmd" {
		return "", fmt.Errorf("gate: %s is a batch file, whose arguments cmd.exe parses again; install the %s program itself", path, GateName)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "check", "--json")
	cmd.Env = envpolicy.Filter(gitenv.WithoutRepoVars(env))
	cmd.Stdin = strings.NewReader(`{"version":"` + GateProtocol + `","args":["git","status"]}`)
	stdout := &boundedBuffer{keepHead: 16 << 10, keepTail: 1 << 10}
	stderr := &boundedBuffer{keepHead: 16 << 10, keepTail: 1 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 5 * time.Second
	runErr := cmd.Run()
	var answer struct {
		Version, Decision, Error string
	}
	decodeErr := json.Unmarshal([]byte(stdout.String()), &answer)
	if runErr == nil && decodeErr == nil && answer.Version == GateProtocol &&
		slices.Contains([]string{"allow", "ask", "refuse"}, answer.Decision) {
		return path, nil
	}
	reason := firstLine(answer.Error, stderr.String(), stdout.String())
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
		return ShellResult{}, fmt.Errorf("refused by hrn: a command that names %s's -approved or -script-b64 flag could approve its own ask, and only a person approves one", GateName)
	}
	shell, _ := Shell()
	argv := []string{ws.Gate, "run", "--shell", shell, "--agent", "hrn"}
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
		return res, fmt.Errorf("the %s gate could not start, so the command did not run: %w", GateName, err)
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
		return res, fmt.Errorf("refused by the %s gate: %s. Do not retry it unchanged; say in your answer why it is needed",
			GateName, strings.TrimSpace(res.Output))
	case slices.Contains(gateNotRun, res.ExitCode) && said:
		return res, fmt.Errorf("the %s gate did not run the command (exit %d): %s", GateName, res.ExitCode, strings.TrimSpace(res.Output))
	}
	return res, nil
}
