package batch

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/tools"
)

func isFakeGate() bool {
	return strings.TrimSuffix(strings.ToLower(filepath.Base(os.Args[0])), ".exe") == tools.GateName
}

// fakeCommandGate stands in for adanext-exec: it answers the handshake unless the file "old" sits
// next to it, logs each run's arguments and policy there, refuses fake-refuse and runs the rest.
func fakeCommandGate(args []string) int {
	exe, err := os.Executable()
	if err != nil || len(args) == 0 {
		return 126
	}
	dir := filepath.Dir(exe)
	switch args[0] {
	case "check":
		if _, err := os.Stat(filepath.Join(dir, "old")); err == nil {
			fmt.Fprintln(os.Stderr, `adanext-exec: unknown command "check"`)
			return 121
		}
		fmt.Println(`{"version":"adanext-exec/1","decision":"allow"}`)
		return 0
	case "run":
		line, _ := json.Marshal(args)
		f, err := os.OpenFile(filepath.Join(dir, "runs.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return 126
		}
		fmt.Fprintf(f, "%s\n", line)
		f.Close()
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		fs.String("shell", "", "")
		timeout := fs.Duration("timeout", 0, "")
		fs.String("agent", "", "")
		fs.String("run-id", "", "")
		policy := fs.String("policy", "", "")
		if fs.Parse(args[1:]) != nil || len(fs.Args()) != 1 {
			return 121
		}
		// Like the real gate, it runs nothing without its policy.
		seen, err := os.ReadFile(*policy)
		if err != nil {
			fmt.Fprintln(os.Stderr, "adanext-exec: refused: the policy cannot be read")
			return 125
		}
		_ = os.WriteFile(filepath.Join(dir, "policy.seen"), seen, 0o644)
		script := fs.Args()[0]
		switch script {
		case "fake-cannot":
			fmt.Fprintln(os.Stderr, "adanext-exec: could not contain the command")
			return 126
		case "fake-refuse":
			fmt.Fprintln(os.Stderr, "adanext-exec: refused by the policy: git-push")
			return 125
		case "fake-utf16":
			os.Stdout.WriteString(gateUTF16("gate says café ✓\n", binary.LittleEndian, true))
			return 2
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		wd, _ := os.Getwd()
		res, err := tools.RunShell(ctx, wd, script, *timeout, nil)
		if err != nil {
			return 126
		}
		os.Stdout.WriteString(res.Output)
		if res.TimedOut {
			return 124
		}
		return res.ExitCode
	}
	return 121
}

// operatorGate is a command gate on the installed fake, with an operator policy whose sum it keeps.
func operatorGate(t *testing.T, dir string) commandGate {
	t.Helper()
	path := filepath.Join(dir, tools.GateName)
	if runtime.GOOS == "windows" {
		path += ".exe"
	}
	policy, done, err := tools.WriteGatePolicy(true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(done)
	sum, err := tools.GatePolicySum(policy)
	if err != nil {
		t.Fatal(err)
	}
	return commandGate{path: path, policy: policy, policySum: sum}
}

// installCommandGate copies the test binary under the gate's name first in PATH and returns its directory.
func installCommandGate(t *testing.T) string {
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

func commandGateRuns(t *testing.T, dir string) [][]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "runs.log"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var runs [][]string
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var args []string
		if err := json.Unmarshal([]byte(line), &args); err != nil {
			t.Fatal(err)
		}
		runs = append(runs, args)
	}
	return runs
}

func argAfter(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func gatedSpec(dir string, gates ...string) Spec {
	return Spec{Dir: dir, Backend: "wb-ok", Options: agent.Options{Gate: true}, Gate: gates,
		Tasks: []Task{{ID: "t", Prompt: "do the thing"}}}
}

func TestBatchGatesRunThroughTheCommandGateWithAnOperatorPolicy(t *testing.T) {
	gate := installCommandGate(t)
	res, _ := run(t, gatedSpec(initRepo(t), "echo gated-ok", "echo names -approved"))
	tr := res.Tasks[0]
	if tr.Status != StatusPass || len(tr.Attempts) != 1 || len(tr.Attempts[0].Gates) != 2 {
		t.Fatalf("task = %+v", tr)
	}
	att := tr.Attempts[0]
	if g := att.Gates[0]; g.ExitCode != 0 || !strings.Contains(g.Output, "gated-ok") || !g.Gated {
		t.Errorf("gate = %+v, want it passed and marked gated", g)
	}
	runs := commandGateRuns(t, gate)
	if len(runs) != 2 {
		t.Fatalf("gate runs = %q, want both gate commands", runs)
	}
	shell, _ := tools.Shell()
	args := runs[0]
	if args[0] != "run" || argAfter(args, "--shell") != shell || argAfter(args, "--agent") != "hrn" ||
		argAfter(args, "--timeout") != defaultGateTimeout.String() || argAfter(args, "--run-id") != att.RunID ||
		argAfter(args, "--policy") == "" || args[len(args)-2] != "--" || args[len(args)-1] != "echo gated-ok" {
		t.Errorf("gate run = %q, want the shell, the gate timeout, the attempt's run id and the policy", args)
	}
	// An operator's command needs no person's approval, so naming the flag is not refused.
	if runs[1][len(runs[1])-1] != "echo names -approved" || att.Gates[1].ExitCode != 0 {
		t.Errorf("second gate = %+v, run %q", att.Gates[1], runs[1])
	}
	seen, err := os.ReadFile(filepath.Join(gate, "policy.seen"))
	if err != nil {
		t.Fatal(err)
	}
	var policy struct {
		Default string
		Rules   []struct{ Name, Decision string }
	}
	if err := json.Unmarshal(seen, &policy); err != nil || policy.Default != "allow" || len(policy.Rules) == 0 {
		t.Fatalf("policy = %s", seen)
	}
	for _, r := range policy.Rules {
		if r.Decision != "allow" || r.Name == "guard-env" || r.Name == "gate-policy-file" {
			t.Errorf("policy rule %+v: gates are operator commands, so no guard rule refuses", r)
		}
	}
	if _, err := os.Stat(argAfter(args, "--policy")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the policy file outlived the task: %v", err)
	}
}

func TestBatchGatesRunDirectlyWithoutTheGateOption(t *testing.T) {
	gate := installCommandGate(t)
	spec := gatedSpec(initRepo(t), "echo plain")
	spec.Options.Gate = false
	res, _ := run(t, spec)
	if g := res.Tasks[0].Attempts[0].Gates[0]; res.Tasks[0].Status != StatusPass || g.Gated {
		t.Fatalf("task = %+v", res.Tasks[0])
	}
	if runs := commandGateRuns(t, gate); runs != nil {
		t.Errorf("gate runs = %q, want none", runs)
	}
}

func TestBatchWithTheGateOnDoesNotStartWhenTheGateFailsTheHandshake(t *testing.T) {
	gate := installCommandGate(t)
	if err := os.WriteFile(filepath.Join(gate, "old"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ := run(t, gatedSpec(initRepo(t), "echo never"))
	tr := res.Tasks[0]
	if tr.Status != StatusError || len(tr.Attempts) != 0 || !strings.Contains(tr.Error, "handshake") {
		t.Fatalf("task = %+v, want it failed before any attempt", tr)
	}
	if runs := commandGateRuns(t, gate); runs != nil {
		t.Errorf("gate runs = %q, want none", runs)
	}
}

func TestBatchGateThroughTheGateKeepsTheTasksTimeout(t *testing.T) {
	gate := installCommandGate(t)
	spec := gatedSpec(initRepo(t), "echo still going; sleep 10")
	spec.GateTimeout = agent.Duration(20 * time.Second)
	spec.Tasks[0].GateTimeout = agent.Duration(time.Second)
	res, _ := run(t, spec)
	tr := res.Tasks[0]
	if tr.Status != StatusFail || len(tr.Attempts) != 1 || len(tr.Attempts[0].Gates) != 1 {
		t.Fatalf("task = %+v", tr)
	}
	g := tr.Attempts[0].Gates[0]
	if g.ExitCode != -1 || g.Killed != KilledTimeout || g.Timeout != agent.Duration(time.Second) ||
		!strings.Contains(g.Output, "timed out after 1s") || g.InfrastructureError != "" || g.Duration > 8*time.Second {
		t.Errorf("gate = %+v, want it killed after the task's 1s", g)
	}
	if runs := commandGateRuns(t, gate); len(runs) != 1 || argAfter(runs[0], "--timeout") != "1s" {
		t.Errorf("gate runs = %q, want the task's timeout passed to the gate", runs)
	}
}

func TestBatchGateExitThroughTheGateIsTheCommandsAndTellsTheRetry(t *testing.T) {
	gate := installCommandGate(t)
	spec := gatedSpec(initRepo(t), "echo broken; exit 3")
	spec.Retry = []string{"wb-ok"}
	res, _ := run(t, spec)
	tr := res.Tasks[0]
	if tr.Status != StatusFail || len(tr.Attempts) != 2 {
		t.Fatalf("task = %+v", tr)
	}
	for i, att := range tr.Attempts {
		if g := att.Gates[0]; g.ExitCode != 3 || g.InfrastructureError != "" || !strings.Contains(g.Output, "broken") || !g.Gated {
			t.Errorf("attempt %d gate = %+v, want the command's exit 3 through the gate", i+1, g)
		}
	}
	// The retry's gate still has the task's policy, which the real gate needs to run anything.
	if runs := commandGateRuns(t, gate); len(runs) != 2 || argAfter(runs[0], "--policy") != argAfter(runs[1], "--policy") {
		t.Errorf("gate runs = %q, want one per attempt under the task's policy", runs)
	}
	if p := tr.Attempts[1].Prompt; !strings.Contains(p, "`echo broken; exit 3` exited 3") || !strings.Contains(p, "broken") {
		t.Errorf("attempt 2 prompt:\n%s", p)
	}
}

func TestBatchGateTheGateRefusesIsAnInfrastructureFailure(t *testing.T) {
	installCommandGate(t)
	spec := gatedSpec(initRepo(t), "fake-refuse")
	spec.Retry = []string{"wb-ok"}
	res, _ := run(t, spec)
	tr := res.Tasks[0]
	if tr.Status != StatusFail || len(tr.Attempts) != 1 {
		t.Fatalf("task = %+v, want no retry", tr)
	}
	g := tr.Attempts[0].Gates[0]
	if g.ExitCode != -1 || !strings.Contains(g.InfrastructureError, "refused by the adanext-exec gate") || !strings.Contains(g.Output, "git-push") ||
		strings.Contains(g.InfrastructureError, "in your answer") {
		t.Errorf("gate = %+v, want the gate's refusal as an infrastructure failure", g)
	}
}

func TestBatchGateThroughTheGateDecodesUTF16(t *testing.T) {
	gate := installCommandGate(t)
	g := runGate(context.Background(), t.TempDir(), "fake-utf16", time.Minute, nil, operatorGate(t, gate))
	if g.ExitCode != 2 || !strings.Contains(g.Output, "gate says café ✓") || !g.Gated {
		t.Errorf("gate = %+v, want decoded UTF-8 and the command's exit", g)
	}
}

func TestBatchGateThroughTheGateCanceledBeforeStartIsCanceled(t *testing.T) {
	gate := installCommandGate(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g := runGate(ctx, t.TempDir(), "echo never", time.Minute, nil, operatorGate(t, gate))
	if g.ExitCode != -1 || g.Killed != KilledCanceled || g.InfrastructureError != "" || g.Outcome() != "canceled" {
		t.Errorf("gate = %+v, want it recorded as canceled", g)
	}
	if runs := commandGateRuns(t, gate); runs != nil {
		t.Errorf("gate runs = %q, want none", runs)
	}
}

func TestBatchGateDoesNotRunOnceItsPolicyWasChanged(t *testing.T) {
	gate := installCommandGate(t)
	through := operatorGate(t, gate)
	if err := os.WriteFile(through.policy, []byte(`{"version":1,"default":"refuse","rules":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	g := runGate(context.Background(), t.TempDir(), "echo never", time.Minute, nil, through)
	if g.ExitCode != -1 || !strings.Contains(g.InfrastructureError, "gate policy file was changed or removed") || !g.Gated {
		t.Errorf("gate = %+v, want an infrastructure failure naming the policy", g)
	}
	if runs := commandGateRuns(t, gate); runs != nil {
		t.Errorf("gate runs = %q, want none", runs)
	}
}

func TestBatchGateTheGateCouldNotRunIsAnInfrastructureFailure(t *testing.T) {
	installCommandGate(t)
	spec := gatedSpec(initRepo(t), "fake-cannot")
	spec.Retry = []string{"wb-ok"}
	res, _ := run(t, spec)
	tr := res.Tasks[0]
	if tr.Status != StatusFail || len(tr.Attempts) != 1 {
		t.Fatalf("task = %+v, want no retry", tr)
	}
	g := tr.Attempts[0].Gates[0]
	if g.ExitCode != -1 || !strings.Contains(g.InfrastructureError, "did not run the command (exit 126)") ||
		!strings.Contains(g.InfrastructureError, "could not contain the command") || strings.Contains(g.InfrastructureError, "shell could not start") {
		t.Errorf("gate = %+v, want the gate's own reason", g)
	}
}

func TestBatchGateThroughTheGateCanceledMidRunIsCanceled(t *testing.T) {
	gate := installCommandGate(t)
	flag := filepath.ToSlash(filepath.Join(t.TempDir(), "gate.started"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			if _, err := os.Stat(flag); err == nil {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	g := runGate(ctx, t.TempDir(), "touch "+flag+" && sleep 30", time.Minute, nil, operatorGate(t, gate))
	if g.ExitCode != -1 || g.Killed != KilledCanceled || g.Timeout != 0 || g.InfrastructureError != "" || g.Outcome() != "canceled" || g.Duration > 20*time.Second {
		t.Errorf("gate = %+v, want it recorded as canceled", g)
	}
}
