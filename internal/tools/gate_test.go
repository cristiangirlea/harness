package tools

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test binary, copied under the gate's name, stands in for adanext-exec: the gate is found
// in PATH by that name and takes the gate's own arguments, so no -test.run flag can select it.
func TestMain(m *testing.M) {
	if isFakeGate() {
		os.Exit(fakeGate(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func isFakeGate() bool {
	return strings.TrimSuffix(strings.ToLower(filepath.Base(os.Args[0])), ".exe") == GateName
}

// fakeGate answers the handshake as the file "mode" next to it says, refuses scripts that start
// with fake-refuse, fake-ask or fake-cannot, and runs every other script through the shell.
func fakeGate(args []string) int {
	exe, err := os.Executable()
	if err != nil {
		return 126
	}
	dir := filepath.Dir(exe)
	mode, _ := os.ReadFile(filepath.Join(dir, "mode"))
	if len(args) == 0 {
		return 121
	}
	switch args[0] {
	case "check":
		req, _ := io.ReadAll(os.Stdin)
		_ = os.WriteFile(filepath.Join(dir, "check.json"), req, 0o644)
		line, _ := json.Marshal(args)
		_ = os.WriteFile(filepath.Join(dir, "check.args"), line, 0o644)
		var sent struct{ Script string }
		_ = json.Unmarshal(req, &sent)
		switch {
		case strings.HasPrefix(sent.Script, "fake-refuse"):
			fmt.Println(`{"version":"adanext-exec/1","decision":"refuse","rule":"git-push"}`)
			return 0
		case strings.HasPrefix(sent.Script, "fake-ask"):
			fmt.Println(`{"version":"adanext-exec/1","decision":"ask"}`)
			return 0
		case strings.HasPrefix(sent.Script, "fake-deny"):
			fmt.Println(`{"version":"adanext-exec/1","decision":"deny"}`)
			return 0
		case strings.HasPrefix(sent.Script, "fake-unusable"):
			fmt.Println(`{"version":"adanext-exec/1","error":"the policy cannot be used"}`)
			return 125
		case strings.HasPrefix(sent.Script, "fake-wedged"):
			time.Sleep(60 * time.Second)
			return 126
		}
		switch string(mode) {
		case "old":
			fmt.Fprintln(os.Stderr, `adanext-exec: unknown command "check"`)
			return 121
		case "v2":
			fmt.Println(`{"version":"adanext-exec/2","decision":"refuse","rule":"git-push"}`)
			return 0
		case "policy":
			fmt.Println(`{"version":"adanext-exec/1","error":"the policy cannot be used: version 9, want 1"}`)
			return 125
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
		shell := fs.String("shell", "", "")
		timeout := fs.Duration("timeout", 0, "")
		fs.String("agent", "", "")
		fs.String("run-id", "", "")
		fs.String("policy", "", "")
		if fs.Parse(args[1:]) != nil || len(fs.Args()) != 1 {
			fmt.Fprintln(os.Stderr, "adanext-exec run: usage")
			return 121
		}
		script := fs.Args()[0]
		switch {
		case strings.HasPrefix(script, "fake-refuse"):
			fmt.Fprintln(os.Stderr, "adanext-exec: refused by the policy: git-push")
			return 125
		case strings.HasPrefix(script, "fake-ask"):
			fmt.Fprintln(os.Stderr, "adanext-exec: refused by the policy: git-push asks a person, and none can be asked here")
			return 125
		case strings.HasPrefix(script, "fake-unusable"):
			fmt.Fprintln(os.Stderr, "adanext-exec: the policy cannot be used")
			return 125
		case strings.HasPrefix(script, "fake-cannot"):
			fmt.Fprintln(os.Stderr, "adanext-exec: could not contain the command")
			return 126
		}
		if name, _ := Shell(); *shell != name {
			fmt.Fprintf(os.Stderr, "adanext-exec: unknown shell %q\n", *shell)
			return 126
		}
		// Like the real gate, a terminate request kills the command's tree; fake-hang ignores the timeout.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		limit := *timeout
		if rest, ok := strings.CutPrefix(script, "fake-hang "); ok {
			script, limit = rest, 0
		}
		if rest, ok := strings.CutPrefix(script, "fake-deaf "); ok {
			// stop also cancels ctx, so the deaf gate runs its command on a fresh one.
			stop()
			signal.Ignore(syscall.SIGTERM)
			ctx = context.Background()
			script, limit = rest, 0
		}
		wd, _ := os.Getwd()
		res, err := RunShell(ctx, wd, script, limit, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "adanext-exec: %v\n", err)
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

// installFakeGate puts the fake gate first in PATH and returns its path.
func installFakeGate(t *testing.T, mode string) string {
	t.Helper()
	dir := t.TempDir()
	name := GateName
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	exe, err := os.Executable()
	must(t, err)
	// A copy, not a link: Windows cannot remove a link to the running test binary.
	path := filepath.Join(dir, name)
	data, err := os.ReadFile(exe)
	must(t, err)
	must(t, os.WriteFile(path, data, 0o755))
	if mode != "" {
		must(t, os.WriteFile(filepath.Join(dir, "mode"), []byte(mode), 0o644))
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

// gatedWorkspace is a workspace whose commands go through a fake gate found by FindGate.
func gatedWorkspace(t *testing.T) (*Workspace, Tool, string) {
	t.Helper()
	ws, tl := setup(t)
	installed := installFakeGate(t, "")
	policy, done, err := WriteGatePolicy(false)
	must(t, err)
	t.Cleanup(done)
	path, err := FindGate(context.Background(), os.Environ(), policy)
	must(t, err)
	ws.Gate, ws.GatePolicy = path, policy
	return ws, tl["run_command"], filepath.Join(filepath.Dir(installed), "runs.log")
}

// script picks the bash/sh or PowerShell form of a command for the shell run_command uses.
func script(posix, powershell string) string {
	if shell, _ := Shell(); shell == "powershell" {
		return powershell
	}
	return posix
}

func gateRuns(t *testing.T, log string) [][]string {
	t.Helper()
	data, err := os.ReadFile(log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	must(t, err)
	var runs [][]string
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var args []string
		must(t, json.Unmarshal([]byte(line), &args))
		runs = append(runs, args)
	}
	return runs
}

func TestFindGateRefusesARunWhenTheGateIsNotInPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := FindGate(context.Background(), os.Environ(), "")
	if err == nil || !strings.Contains(err.Error(), "adanext-exec is not in PATH") {
		t.Fatalf("FindGate = %v, want a missing gate", err)
	}
}

func TestFindGateAcceptsAGateThatSpeaksProtocolOne(t *testing.T) {
	installed := installFakeGate(t, "")
	path, err := FindGate(context.Background(), os.Environ(), "")
	must(t, err)
	got, err := filepath.EvalSymlinks(path)
	must(t, err)
	want, err := filepath.EvalSymlinks(installed)
	must(t, err)
	if !strings.EqualFold(got, want) {
		t.Fatalf("FindGate = %s, want %s", got, want)
	}
	req, err := os.ReadFile(filepath.Join(filepath.Dir(installed), "check.json"))
	must(t, err)
	var sent struct{ Version string }
	if json.Unmarshal(req, &sent) != nil || sent.Version != GateProtocol {
		t.Fatalf("handshake request = %s, want version %s", req, GateProtocol)
	}
}

func TestFindGateRefusesAnOlderOrOtherGate(t *testing.T) {
	for mode, want := range map[string]string{
		"old":    `unknown command "check"`,
		"v2":     "adanext-exec/2",
		"policy": "the policy cannot be used",
	} {
		t.Run(mode, func(t *testing.T) {
			installFakeGate(t, mode)
			_, err := FindGate(context.Background(), os.Environ(), "")
			if err == nil || !strings.Contains(err.Error(), "handshake") || !strings.Contains(err.Error(), want) {
				t.Fatalf("FindGate = %v, want a failed handshake naming %q", err, want)
			}
		})
	}
}

func TestFindGateRefusesABatchFile(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("batch files are Windows only")
	}
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, GateName+".cmd"), []byte("@echo off\r\n"), 0o755))
	t.Setenv("PATH", dir)
	_, err := FindGate(context.Background(), os.Environ(), "")
	if err == nil || !strings.Contains(err.Error(), "batch file") {
		t.Fatalf("FindGate = %v, want a refused batch file", err)
	}
}

func TestGatedRunCommandNamesTheShellTimeoutAndRun(t *testing.T) {
	ws, tl, log := gatedWorkspace(t)
	ws.RunDir = filepath.Join(t.TempDir(), "runs", "r20261107")
	out, err := call(t, ws, tl, map[string]any{"command": "echo hi", "timeout_sec": 30})
	if err != nil || out != "hi\n\n[exit code 0]" {
		t.Fatalf("run_command = %q, %v", out, err)
	}
	shell, _ := Shell()
	want := []string{"run", "--shell", shell, "--agent", "hrn", "--policy", ws.GatePolicy, "--timeout", "30s", "--run-id", "r20261107", "--", "echo hi"}
	if runs := gateRuns(t, log); len(runs) != 1 || strings.Join(runs[0], "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("gate ran with %q, want %q", runs, want)
	}
}

func TestGatedRunCommandPassesOutputAndExitCodeThrough(t *testing.T) {
	ws, tl, _ := gatedWorkspace(t)
	out, err := call(t, ws, tl, map[string]any{"command": script(
		"echo out; echo err >&2; exit 3",
		"[Console]::Out.WriteLine('out'); [Console]::Error.WriteLine('err'); exit 3")})
	if err == nil || err.Error() != "exit code 3" {
		t.Fatalf("error = %v, want exit code 3", err)
	}
	if !strings.Contains(out, "out\n") || !strings.Contains(out, "err\n") || !strings.HasSuffix(out, "[exit code 3]") {
		t.Fatalf("output = %q", out)
	}
}

func TestGatedRunCommandKeepsTheCommandsOwnGateLikeExitCodes(t *testing.T) {
	ws, tl, _ := gatedWorkspace(t)
	for _, code := range []int{120, 121, 123, 124, 125, 126, 127} {
		out, err := call(t, ws, tl, map[string]any{"command": fmt.Sprintf("echo own; exit %d", code), "timeout_sec": 60})
		want := fmt.Sprintf("exit code %d", code)
		if err == nil || err.Error() != want || out != "own\n\n["+want+"]" {
			t.Errorf("exit %d: run_command = %q, %v; want the command's own code", code, out, err)
		}
	}
}

func TestGatedRunCommandReportsTheGatesRefusal(t *testing.T) {
	for script, want := range map[string]string{
		"fake-refuse make release": "refused by the policy: git-push",
		"fake-ask make release":    "git-push asks a person, and none can be asked here",
	} {
		ws, tl, _ := gatedWorkspace(t)
		out, err := call(t, ws, tl, map[string]any{"command": script})
		if out != "" || err == nil || !strings.Contains(err.Error(), "refused by the adanext-exec gate") || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: run_command = %q, %v; want a refusal naming %q", script, out, err, want)
		}
	}
}

func TestGatedRunCommandFailsClosedWhenTheGateCannotRunIt(t *testing.T) {
	ws, tl, _ := gatedWorkspace(t)
	out, err := call(t, ws, tl, map[string]any{"command": "fake-cannot run this"})
	if out != "" || err == nil || !strings.Contains(err.Error(), "did not run the command (exit 126)") ||
		!strings.Contains(err.Error(), "could not contain the command") {
		t.Fatalf("run_command = %q, %v; want the gate's failure", out, err)
	}
}

func TestGatedRunCommandNeverRunsUngatedWhenTheGateGoesMissing(t *testing.T) {
	ws, tl, _ := gatedWorkspace(t)
	must(t, os.Remove(ws.Gate))
	out, err := call(t, ws, tl, map[string]any{"command": script("echo ran > marker.txt", "Set-Content marker.txt ran")})
	if err == nil || !strings.Contains(err.Error(), "adanext-exec gate") {
		t.Fatalf("run_command = %q, %v; want a failure naming the gate", out, err)
	}
	if _, statErr := os.Stat(filepath.Join(ws.Root, "marker.txt")); statErr == nil {
		t.Fatal("the command ran without the gate")
	}
}

func TestGatedRunCommandMapsTheGatesTimeout(t *testing.T) {
	ws, tl, _ := gatedWorkspace(t)
	start := time.Now()
	out, err := call(t, ws, tl, map[string]any{"command": script("echo started; sleep 60", "'started'; Start-Sleep -Seconds 60"), "timeout_sec": 3})
	if err == nil || err.Error() != "command timed out" || !strings.HasSuffix(out, "[timed out after 3s]") {
		t.Fatalf("run_command = %q, %v; want a timeout", out, err)
	}
	if took := time.Since(start); took > 40*time.Second {
		t.Fatalf("the timeout took %s; the gate should have killed the command at 3s", took)
	}
}

func TestGatedRunCommandKillsAGateThatOverstaysTheTimeout(t *testing.T) {
	old := gateGrace
	gateGrace = time.Second
	t.Cleanup(func() { gateGrace = old })
	ws, tl, _ := gatedWorkspace(t)
	start := time.Now()
	out, err := call(t, ws, tl, map[string]any{"command": "fake-hang " + script("sleep 60", "Start-Sleep -Seconds 60"), "timeout_sec": 2})
	if err == nil || err.Error() != "command timed out" || !strings.HasSuffix(out, "[timed out after 2s]") {
		t.Fatalf("run_command = %q, %v; want a timeout", out, err)
	}
	// Two seconds of timeout, one of grace, and at most WaitDelay for the gate to end.
	if took := time.Since(start); took > 20*time.Second {
		t.Fatalf("the hanging gate was ended after %s", took)
	}
}

func TestGatedRunCommandRefusesSelfApprovalBeforeTheGate(t *testing.T) {
	ws, tl, log := gatedWorkspace(t)
	for _, dangerous := range []bool{false, true} {
		ws.AllowDangerous = dangerous
		for _, command := range []string{
			"adanext-exec run --approved=make-release --shell bash -- 'make release'",
			"adanext-exec run -approved make-release -- make",
			"adanext-exec run --shell bash --script-b64 bWFrZQ",
		} {
			out, err := call(t, ws, tl, map[string]any{"command": command})
			if out != "" || err == nil || !strings.Contains(err.Error(), "only a person approves one") {
				t.Errorf("allow_dangerous=%v %s: run_command = %q, %v; want a refusal", dangerous, command, out, err)
			}
		}
	}
	if runs := gateRuns(t, log); len(runs) != 0 {
		t.Fatalf("the gate was called for a self-approval: %q", runs)
	}
}

func TestGatedRunCommandRunsInTheGuardedEnvironment(t *testing.T) {
	if shell, _ := Shell(); shell == "powershell" {
		t.Skip("the check reads variables with POSIX syntax")
	}
	// Only the guard withholds this name; the filter every command gets keeps it.
	t.Setenv("GATE_TEST_PASSWORD", "a-secret")
	ws, tl, _ := gatedWorkspace(t)
	out, err := call(t, ws, tl, map[string]any{"command": `echo "token=${GATE_TEST_PASSWORD:-withheld} guarded=$HRN_GUARDED"`})
	if err != nil || !strings.Contains(out, "token=withheld guarded=1") {
		t.Fatalf("run_command = %q, %v; want the guarded environment", out, err)
	}
}

func TestGatedRunCommandHandsTheGateTheCommandTextUnchanged(t *testing.T) {
	ws, tl, log := gatedWorkspace(t)
	command := "echo \"a b\" \\\"q\\\" 'tr\\' %PATH% '$x' \"\"\necho second\\"
	_, _ = call(t, ws, tl, map[string]any{"command": command})
	runs := gateRuns(t, log)
	if len(runs) != 1 || runs[0][len(runs[0])-1] != command || runs[0][len(runs[0])-2] != "--" {
		t.Fatalf("the gate received %q, want %q after --", runs, command)
	}
}

func TestAGateWithNoPolicyKeepsHrnsGuardFirst(t *testing.T) {
	ws, tl, log := gatedWorkspace(t)
	ws.GatePolicy = ""
	_, err := call(t, ws, tl, map[string]any{"command": "sudo rm -rf /"})
	if err == nil || !strings.Contains(err.Error(), "hrn guard") {
		t.Fatalf("error = %v, want the hrn guard", err)
	}
	if runs := gateRuns(t, log); len(runs) != 0 {
		t.Fatalf("the gate was called for a guarded command: %q", runs)
	}
}
