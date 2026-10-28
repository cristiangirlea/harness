package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Shell prefers bash (Git Bash), then PowerShell or sh.
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
	return runtime.GOOS == "windows" && inSystem32(path)
}

func inSystem32(path string) bool {
	dir := path[:max(strings.LastIndexAny(path, `\/`), 0)]
	return strings.EqualFold(dir[strings.LastIndexAny(dir, `\/`)+1:], "System32")
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
		Refuse: func(ws *Workspace, raw json.RawMessage) string {
			args, err := decode[in](raw)
			if err != nil || ws.AllowDangerous {
				return "" // a call that cannot be read is the loop's to report
			}
			return Guard(args.Command)
		},
		Run: func(ctx context.Context, ws *Workspace, raw json.RawMessage) (string, error) {
			args, err := decode[in](raw)
			if err != nil {
				return "", err
			}
			// Direct callers bypass Refuse; recheck here.
			if !ws.AllowDangerous {
				if rule := Guard(args.Command); rule != "" {
					return "", GuardError(rule)
				}
			}
			timeout := 120 * time.Second
			if args.TimeoutSec > 0 {
				timeout = time.Duration(min(args.TimeoutSec, 600)) * time.Second
			}
			// Keys must not reach model output or enable publishing.
			var env []string
			if !ws.AllowDangerous {
				guardDir, err := os.MkdirTemp("", "hrn-guard-*")
				if err != nil {
					return "", err
				}
				defer os.RemoveAll(guardDir)
				env, _ = GuardEnv(os.Environ(), guardDir)
			}
			if env == nil {
				env = os.Environ()
			}
			env = append(env, ws.ChildEnv...)
			res, err := runShell(ctx, ws.Root, args.Command, timeout, env, ws.ShapeTools)
			if err != nil {
				return "", err
			}
			if res.OutputCut {
				res.Output += fmt.Sprintf("\n[output exceeded %d-byte capture ceiling; original unavailable]", maxCapture)
			} else if ws.ShapeTools {
				res.Output = ws.toolOutput(res.Output)
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
