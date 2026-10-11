package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestHelperCLIError is not a test: the test binary acts as an agent CLI that
// reports something on its stream and then exits non-zero.
func TestHelperCLIError(t *testing.T) {
	switch os.Getenv("HRN_CLI_HELPER") {
	case "error":
		fmt.Println(`{"type":"error","error":{"message":"usage limit reached"}}`)
		os.Exit(1)
	case "sleep":
		time.Sleep(10 * time.Second)
		os.Exit(0)
	case "maxturns":
		fmt.Println(`{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":3,"result":""}`)
		os.Exit(1)
	case "env":
		// What the CLI was started with, as its final answer.
		report := fmt.Sprintf("GH_TOKEN=%s;GH_CONFIG_DIR=%s;GIT_CONFIG_COUNT=%s;ANTHROPIC_API_KEY=%s;CLAUDE_CODE_OAUTH_TOKEN=%s",
			os.Getenv("GH_TOKEN"), os.Getenv("GH_CONFIG_DIR"), os.Getenv("GIT_CONFIG_COUNT"), os.Getenv("ANTHROPIC_API_KEY"), os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"))
		out, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "num_turns": 1, "result": report})
		fmt.Println(string(out))
		os.Exit(0)
	}
}

// Every CLI backend runs under the guard: the publishing credentials are not
// in its environment, git cannot push and gh has an empty login of its own,
// removed when the run ends. --allow-dangerous lifts it.
func TestACLIRunsUnderTheGuard(t *testing.T) {
	t.Setenv("HRN_CLI_HELPER", "env")
	t.Setenv("GH_TOKEN", "ghp_secret")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-metered")
	t.Setenv("GH_CONFIG_DIR", "operator-gh")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "claude-plan-login")
	saw := func(b *CLIBackend, opts Options) map[string]string {
		res := b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: opts}, func(Event) {})
		got := map[string]string{}
		for _, kv := range strings.Split(res.Final, ";") {
			k, v, _ := strings.Cut(kv, "=")
			got[k] = v
		}
		return got
	}
	got := saw(helperCLI(parseClaudeStream), Options{Mode: ModeYolo})
	if got["GH_TOKEN"] != "" || got["ANTHROPIC_API_KEY"] != "" || got["GIT_CONFIG_COUNT"] == "" ||
		got["GH_CONFIG_DIR"] == "" || got["GH_CONFIG_DIR"] == "operator-gh" {
		t.Fatalf("the CLI saw %v; want no GH_TOKEN, an empty gh config dir of the run's own and git's push guard", got)
	}
	if _, err := os.Stat(got["GH_CONFIG_DIR"]); !os.IsNotExist(err) {
		t.Errorf("gh's empty config dir %s outlived the run: %v", got["GH_CONFIG_DIR"], err)
	}
	// Claude's plan login goes to the claude CLI and to no other.
	if got["CLAUDE_CODE_OAUTH_TOKEN"] != "" {
		t.Errorf("a CLI other than claude saw claude's plan login")
	}
	claude := helperCLI(parseClaudeStream)
	claude.loginEnv = NewClaudeBackend().loginEnv
	if got := saw(claude, Options{Mode: ModeYolo}); got["CLAUDE_CODE_OAUTH_TOKEN"] != "claude-plan-login" {
		t.Errorf("the claude CLI lost its plan login under the guard: %v", got)
	}

	got = saw(helperCLI(parseClaudeStream), Options{Mode: ModeYolo, AllowDangerous: true})
	if got["GH_TOKEN"] != "ghp_secret" || got["GH_CONFIG_DIR"] != "operator-gh" {
		t.Fatalf("with --allow-dangerous the CLI saw %v; want the operator's environment as it is", got)
	}
}

func helperCLI(parse func([]byte, func(Event), *Result) bool) *CLIBackend {
	return &CLIBackend{
		name:        os.Args[0],
		promptStdin: true,
		args:        func(Task, string) []string { return []string{"-test.run=^TestHelperCLIError$", "--"} },
		parse:       parse,
	}
}

func TestCLIKeepsStreamErrorOnNonZeroExit(t *testing.T) {
	t.Setenv("HRN_CLI_HELPER", "error")
	res := helperCLI(parseOpencodeStream).Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Mode: ModeAsk}}, func(Event) {})
	if res.Status != StatusError || !strings.Contains(res.Error, "usage limit reached") {
		t.Fatalf("the stream's error should survive a non-zero exit: %+v", res)
	}
}

func TestCLIKeepsStreamStatusOnNonZeroExit(t *testing.T) {
	t.Setenv("HRN_CLI_HELPER", "maxturns")
	res := helperCLI(parseClaudeStream).Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Mode: ModeAsk}}, func(Event) {})
	if res.Status != StatusMaxTurns || res.Error != "error_max_turns" || res.Turns != 3 {
		t.Fatalf("a status the stream set should survive a non-zero exit: %+v", res)
	}
}

func TestCLISaysWhatItCannotEnforce(t *testing.T) {
	t.Setenv("HRN_CLI_HELPER", "error")
	var infos []string
	b := helperCLI(parseOpencodeStream)
	b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Mode: ModeAsk, MaxCostUSD: 5, TaskBudget: 50000}},
		func(e Event) {
			if e.Kind == EventInfo && strings.Contains(e.Text, "not enforced") {
				infos = append(infos, e.Text)
			}
		})
	if len(infos) != 1 {
		t.Fatalf("want one line about unenforced limits, got %v", infos)
	}
	if !strings.Contains(infos[0], "max_cost") || !strings.Contains(infos[0], "task_budget") {
		t.Errorf("info = %q, want both named", infos[0])
	}
}

// Every CLI run says whether the guard is on, in any mode: a run in auto can
// still run commands the CLI itself allows.
func TestCLISaysWhetherTheGuardApplies(t *testing.T) {
	t.Setenv("HRN_CLI_HELPER", "error")
	guardLines := func(b *CLIBackend, opts Options) []string {
		var out []string
		b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: opts},
			func(e Event) {
				if e.Kind == EventInfo && strings.HasPrefix(e.Text, "guard:") {
					out = append(out, e.Text)
				}
			})
		return out
	}
	for _, mode := range []Mode{ModeYolo, ModeAuto, ModeReadOnly} {
		if lines := guardLines(helperCLI(parseOpencodeStream), Options{Mode: mode}); len(lines) != 1 || !strings.Contains(lines[0], "pointed at a dead end") {
			t.Errorf("mode %s: %v; want one line saying the guard is on", mode, lines)
		}
	}
	if lines := guardLines(helperCLI(parseOpencodeStream), Options{Mode: ModeYolo, AllowDangerous: true}); len(lines) != 1 || !strings.Contains(lines[0], "lifted") {
		t.Errorf("--allow-dangerous: %v; want one line saying the guard is lifted", lines)
	}
}

// What the guard leaves open differs by CLI, and each run says it: codex,
// gemini and opencode take no rules for secret files; claude does.
func TestEachCLIsGuardLineSaysWhatItLeavesOpen(t *testing.T) {
	t.Setenv("HRN_CLI_HELPER", "error")
	for _, b := range []*CLIBackend{NewCodexBackend(), NewGeminiBackend(), NewOpencodeBackend(), NewClaudeBackend()} {
		line := b.guardLine(nil)
		open := strings.Contains(line, "unguarded on the "+b.name+" backend: reading and writing secret files")
		if b.fileRules == open {
			t.Errorf("%s: %q; want the secret files named as unguarded exactly when the CLI takes no file rules", b.name, line)
		}
		if b.fileRules && !strings.Contains(line, "own rules refuse publishing commands and secret files") {
			t.Errorf("%s: %q; want the CLI's own rules named", b.name, line)
		}
	}
	if !NewClaudeBackend().fileRules {
		t.Error("claude hands the secret files to its deny rules")
	}
	// And a run emits that line, not a generic one. The helper is found by
	// its name, which is the test binary's path.
	b := helperCLI(parseOpencodeStream)
	var lines []string
	b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Mode: ModeYolo}}, func(e Event) {
		if e.Kind == EventInfo && strings.HasPrefix(e.Text, "guard:") {
			lines = append(lines, e.Text)
		}
	})
	if len(lines) != 1 || !strings.Contains(lines[0], "unguarded on the "+b.name+" backend") {
		t.Errorf("the run said %v", lines)
	}
}

func TestCLIStopsOnTimeout(t *testing.T) {
	t.Setenv("HRN_CLI_HELPER", "sleep")
	res := helperCLI(parseOpencodeStream).Run(context.Background(),
		Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Mode: ModeAsk, Timeout: Duration(200 * time.Millisecond)}}, func(Event) {})
	if res.Status != StatusTimeout {
		t.Fatalf("status = %s, want timeout (error %q)", res.Status, res.Error)
	}
}
