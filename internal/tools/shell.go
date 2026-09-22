package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// Shell returns the command interpreter used by run_command: bash when one is
// available (Git Bash on Windows), otherwise PowerShell or sh.
func Shell() (name string, argv func(cmd string) []string) {
	if path, err := exec.LookPath("bash"); err == nil && !isWSLLauncher(path) {
		return "bash", func(c string) []string { return []string{path, "-c", c} }
	}
	if runtime.GOOS == "windows" {
		for _, p := range []string{`C:\Program Files\Git\bin\bash.exe`, `C:\Program Files\Git\usr\bin\bash.exe`} {
			if _, err := os.Stat(p); err == nil {
				return "bash", func(c string) []string { return []string{p, "-c", c} }
			}
		}
		return "powershell", func(c string) []string {
			return []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", c}
		}
	}
	return "sh", func(c string) []string { return []string{"/bin/sh", "-c", c} }
}

// System32\bash.exe starts WSL, which is a different filesystem view.
func isWSLLauncher(path string) bool {
	return runtime.GOOS == "windows" && filepath.Base(filepath.Dir(path)) == "System32"
}

func runCommand() Tool {
	type in struct {
		Command    string `json:"command"`
		TimeoutSec int    `json:"timeout_sec"`
	}
	shell, _ := Shell()
	return Tool{
		Name:        "run_command",
		Description: fmt.Sprintf("Run a shell command (%s) in the workspace root and return combined stdout/stderr and the exit code. Non-interactive only. Default timeout 120s, max 600s. A few destructive or publishing command lines are refused by the harness.", shell),
		Properties: map[string]any{
			"command":     str("The command line to run."),
			"timeout_sec": num("Timeout in seconds."),
		},
		Required: []string{"command"},
		Effect:   EffectExec,
		Summary:  summaryOf("command"),
		Run: func(ctx context.Context, ws *Workspace, raw json.RawMessage) (string, error) {
			args, err := decode[in](raw)
			if err != nil {
				return "", err
			}
			if !ws.AllowDangerous {
				if rule := Guard(args.Command); rule != "" {
					return "", GuardError(rule)
				}
			}
			timeout := 120 * time.Second
			if args.TimeoutSec > 0 {
				timeout = time.Duration(min(args.TimeoutSec, 600)) * time.Second
			}
			res, err := RunShell(ctx, ws.Root, args.Command, timeout)
			if err != nil {
				return "", err
			}
			if res.TimedOut {
				return res.Output + fmt.Sprintf("\n[timed out after %s]", timeout), errors.New("command timed out")
			}
			result := res.Output
			if result == "" {
				result = "(no output)"
			}
			result += fmt.Sprintf("\n[exit code %d]", res.ExitCode)
			if res.ExitCode != 0 {
				return result, fmt.Errorf("exit code %d", res.ExitCode)
			}
			return result, nil
		},
	}
}
