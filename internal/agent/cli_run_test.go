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
	case "codexlimit":
		// What codex 0.155 printed on 2026-10-11: the message on an error
		// line, then a turn.failed whose text sits under error.message.
		fmt.Println(`{"type":"thread.started","thread_id":"019a0000-0000-7000-8000-000000000001"}`)
		fmt.Println(`{"type":"error","message":"You’ve hit your usage limit. Visit https://chatgpt.com/settings/usage to purchase more credits or try again at Oct 16th, 2099 3:29 PM."}`)
		fmt.Println(`{"type":"turn.failed","error":{"message":""}}`)
		os.Exit(1)
	case "codexreconnect", "codexreconnectend", "codexreconnectfailed", "codexreconnectafterturn":
		// What codex printed in run 20261014-180728-7102ef: a dropped model
		// stream is an error line the CLI retries by itself, and the turn
		// went on to complete. "codexreconnectend" stops at the notice;
		// "codexreconnectfailed" fails the turn with no text of its own;
		// "codexreconnectafterturn" completes a turn with usage first and
		// then stops at the notice.
		fmt.Println(`{"type":"thread.started","thread_id":"019a0000-0000-7000-8000-000000000002"}`)
		if os.Getenv("HRN_CLI_HELPER") == "codexreconnectafterturn" {
			fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"first"}}`)
			fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":2}}`)
		}
		fmt.Println(`{"type":"item.started","item":{"type":"command_execution","command":"go test ./..."}}`)
		fmt.Println(`{"type":"item.completed","item":{"type":"command_execution","command":"go test ./...","aggregated_output":"ok\n","exit_code":0}}`)
		fmt.Println(`{"type":"error","message":"Reconnecting... 2/5 (We’re currently experiencing high demand, which may cause temporary errors.)"}`)
		if os.Getenv("HRN_CLI_HELPER") == "codexreconnectend" || os.Getenv("HRN_CLI_HELPER") == "codexreconnectafterturn" {
			os.Exit(0)
		}
		if os.Getenv("HRN_CLI_HELPER") == "codexreconnectfailed" {
			fmt.Println(`{"type":"turn.failed","error":{"message":""}}`)
			os.Exit(0)
		}
		fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"done"}}`)
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":2}}`)
		os.Exit(0)
	case "claudelimit":
		// The test picks the reset time, so the run does not depend on the hour.
		msg, _ := json.Marshal(os.Getenv("HRN_CLI_LIMIT_MSG"))
		fmt.Printf(`{"type":"result","subtype":"success","is_error":true,"num_turns":1,"result":%s}`+"\n", msg)
		os.Exit(1)
	case "env":
		// What the CLI was started with, as its final answer.
		report := fmt.Sprintf("GH_TOKEN=%s;GH_CONFIG_DIR=%s;GIT_CONFIG_COUNT=%s;ANTHROPIC_API_KEY=%s;CLAUDE_CODE_OAUTH_TOKEN=%s;HRN_GUARDED=%s",
			os.Getenv("GH_TOKEN"), os.Getenv("GH_CONFIG_DIR"), os.Getenv("GIT_CONFIG_COUNT"), os.Getenv("ANTHROPIC_API_KEY"), os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"),
			os.Getenv("HRN_GUARDED"))
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
	// gh gets a token that is not one, so it does not ask the system's
	// credential store for the operator's.
	if got["GH_TOKEN"] == "ghp_secret" || got["GH_TOKEN"] == "" || got["ANTHROPIC_API_KEY"] != "" || got["GIT_CONFIG_COUNT"] == "" ||
		got["GH_CONFIG_DIR"] == "" || got["GH_CONFIG_DIR"] == "operator-gh" || got["HRN_GUARDED"] != "1" {
		t.Fatalf("the CLI saw %v; want gh's stand-in token, an empty gh config dir of the run's own, git's push guard and the guard's marker", got)
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
	if got["GH_TOKEN"] != "ghp_secret" || got["GH_CONFIG_DIR"] != "operator-gh" || got["HRN_GUARDED"] != "" {
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

func TestCLIReportsEffortOnlyWhenTheAdapterCannotApplyIt(t *testing.T) {
	t.Setenv("HRN_CLI_HELPER", "error")
	for _, adapter := range []struct {
		newBackend func() *CLIBackend
		mapsEffort bool
		mapsLean   bool
		// mapsCompact: the adapter passes compact_window on.
		mapsCompact bool
	}{
		{NewCodexBackend, true, true, false},
		{NewClaudeBackend, true, true, true},
		{NewGeminiBackend, false, false, false},
		{NewOpencodeBackend, false, false, false},
	} {
		b := adapter.newBackend()
		if AppliesLean(b) != adapter.mapsLean {
			t.Errorf("%s: AppliesLean = %v, want %v", b.Name(), AppliesLean(b), adapter.mapsLean)
		}
		t.Run(b.Name(), func(t *testing.T) {
			// Keep the adapter's capabilities, but run only the test binary.
			b.name = os.Args[0]
			b.candidates = nil
			b.args = helperCLI(nil).args
			for _, options := range []Options{
				{},
				{Effort: "high"},
				{Effort: "high", MaxCostUSD: 5, TaskBudget: 50000},
				{Lean: Bool(true)},
				{Effort: "high", Lean: Bool(true)},
				{Lean: Bool(false)},
				{CompactWindow: "auto"},
				{Lean: Bool(true), CompactWindow: "200000"},
			} {
				var infos []string
				res := b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: options}, func(e Event) {
					if e.Kind == EventInfo && strings.Contains(e.Text, "not enforced") {
						infos = append(infos, e.Text)
					}
				})
				// The fake says "usage limit reached", which codex reads as its
				// plan's limit: limited or error, it ran and exited non-zero.
				if res.Status != StatusError && res.Status != StatusLimited {
					t.Fatalf("the fake CLI must run and exit non-zero: %+v", res)
				}
				var want []string
				if options.MaxCostUSD > 0 {
					want = append(want, "max_cost", "task_budget")
				}
				if options.Effort != "" && !adapter.mapsEffort {
					want = append(want, "effort")
				}
				if options.Lean != nil && *options.Lean && !adapter.mapsLean {
					want = append(want, "lean")
				}
				if options.CompactWindow != "" && !adapter.mapsCompact {
					want = append(want, "compact_window")
				}
				if len(want) == 0 {
					if len(infos) != 0 {
						t.Errorf("options %+v: unexpected warning %v", options, infos)
					}
					continue
				}
				line := fmt.Sprintf("%s: not enforced by the %s backend", strings.Join(want, ", "), b.Name())
				if len(infos) != 1 || infos[0] != line {
					t.Errorf("options %+v: info = %v, want one line %q", options, infos, line)
				}
			}
		})
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

func TestCodexReportsItsUsageLimitAsLimited(t *testing.T) {
	t.Setenv("HRN_CLI_HELPER", "codexlimit")
	b := helperCLI(parseCodexStream)
	b.limits = true
	res := b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Mode: ModeAuto}}, func(Event) {})
	if res.Status != StatusLimited {
		t.Fatalf("status = %s, want limited (error %q)", res.Status, res.Error)
	}
	// The year is far off so the test does not age; past the plan window, so
	// the time is not believed, but the message itself is kept.
	if !strings.Contains(res.Error, "hit your usage limit") {
		t.Errorf("error = %q, want codex's own message rather than the exit status", res.Error)
	}
}

func TestCodexATurnThatCompletesAfterAReconnectNoticeIsOK(t *testing.T) {
	t.Setenv("HRN_CLI_HELPER", "codexreconnect")
	b := helperCLI(parseCodexStream)
	b.end = NewCodexBackend().end
	var notices, errs []string
	res := b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Mode: ModeAuto}}, func(e Event) {
		switch {
		case e.Kind == EventError:
			errs = append(errs, e.Text)
		case e.Kind == EventInfo && strings.HasPrefix(e.Text, "Reconnecting..."):
			notices = append(notices, e.Text)
		}
	})
	if res.Status != StatusOK || res.Error != "" || res.Final != "done" {
		t.Fatalf("status %s, error %q, final %q: the CLI retried its model stream and the turn completed", res.Status, res.Error, res.Final)
	}
	if res.Usage == nil || res.Usage.Status != "reported" || res.InputTokens != 10 || res.CacheRead != 4 || res.OutputTokens != 2 {
		t.Errorf("usage %+v, tokens %d/%d/%d: the notice must not lose the turn's counters", res.Usage, res.InputTokens, res.CacheRead, res.OutputTokens)
	}
	if len(errs) != 0 || len(notices) != 1 {
		t.Errorf("error events %q, notices %q: the notice is an info event and no error", errs, notices)
	}
}

func TestCodexAStreamThatEndsOnAReconnectNoticeIsAnError(t *testing.T) {
	// The CLI exits clean with no turn.completed: the notice is the only word
	// on why the turn never finished.
	t.Setenv("HRN_CLI_HELPER", "codexreconnectend")
	b := helperCLI(parseCodexStream)
	// The real backend's end-of-stream step, so the test fails if it is unwired.
	b.end = NewCodexBackend().end
	var errs []string
	var last Event
	res := b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Mode: ModeAuto}}, func(e Event) {
		last = e
		if e.Kind == EventError {
			errs = append(errs, e.Text)
		}
	})
	if res.Status != StatusError || !strings.HasPrefix(res.Error, "Reconnecting...") {
		t.Fatalf("status %s, error %q: a turn that never completed after the notice failed", res.Status, res.Error)
	}
	// The notice itself was shown as information and the CLI exited 0, so
	// without this event the transcript would never say the run failed.
	if len(errs) != 1 || errs[0] != res.Error || last.Kind != EventError {
		t.Errorf("error events %q (last event %s), want one with the run's error %q, at the end", errs, last.Kind, res.Error)
	}
	if res.Usage == nil || res.Usage.InputSemantics != "inclusive_cache" || res.Usage.Status != "missing" || res.Usage.CostStatus != "missing" {
		t.Errorf("usage %+v: no turn reported any, as after turn.failed", res.Usage)
	}
}

func TestCodexAStreamThatEndsOnAReconnectNoticeLosesItsUsage(t *testing.T) {
	// A turn reported its usage, then the next one died on the notice: the
	// run's counters are not whole, the way turn.failed marks them.
	t.Setenv("HRN_CLI_HELPER", "codexreconnectafterturn")
	b := helperCLI(parseCodexStream)
	b.end = NewCodexBackend().end
	var errs []string
	res := b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Mode: ModeAuto, Model: "gpt-6-luna"}}, func(e Event) {
		if e.Kind == EventError {
			errs = append(errs, e.Text)
		}
	})
	if res.Status != StatusError || !strings.HasPrefix(res.Error, "Reconnecting...") || len(errs) != 1 || errs[0] != res.Error {
		t.Fatalf("status %s, error %q, error events %q: the second turn never completed", res.Status, res.Error, errs)
	}
	if res.Usage == nil || res.Usage.InputSemantics != "inclusive_cache" || res.Usage.Status != "missing" || res.Usage.CostStatus != "missing" || res.CostUSD != 0 || res.CostEstimate {
		t.Errorf("usage %+v, cost %v (estimate %v): the last turn's tokens were never reported", res.Usage, res.CostUSD, res.CostEstimate)
	}
	if res.InputTokens != 10 || res.CacheRead != 4 || res.OutputTokens != 2 {
		t.Errorf("tokens %d/%d/%d: the first turn's counters are kept, as after turn.failed", res.InputTokens, res.CacheRead, res.OutputTokens)
	}
}

func TestCodexAFailedTurnAfterAReconnectNoticeIsReportedOnce(t *testing.T) {
	// The turn.failed line already reported the notice as the failure; the
	// end of the stream must not say it again.
	t.Setenv("HRN_CLI_HELPER", "codexreconnectfailed")
	b := helperCLI(parseCodexStream)
	b.end = NewCodexBackend().end
	var errs []string
	res := b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Mode: ModeAuto}}, func(e Event) {
		if e.Kind == EventError {
			errs = append(errs, e.Text)
		}
	})
	if res.Status != StatusError || !strings.HasPrefix(res.Error, "Reconnecting...") {
		t.Fatalf("status %s, error %q: the failed turn's reason is the notice", res.Status, res.Error)
	}
	if len(errs) != 1 || errs[0] != res.Error {
		t.Errorf("error events %q, want exactly one with %q", errs, res.Error)
	}
	if res.Usage == nil || res.Usage.Status != "missing" || res.Usage.CostStatus != "missing" {
		t.Errorf("usage %+v, want missing once the turn failed", res.Usage)
	}
}

// claudeLimitIn sets the fake claude's limit message to reset d from now, in
// the computer's zone as the CLI writes it. The zone is pinned to one without
// summer time, where a clock time names one moment even the night it changes.
func claudeLimitIn(t *testing.T, d time.Duration) time.Time {
	saved := time.Local
	time.Local = time.FixedZone("EET", 2*3600)
	t.Cleanup(func() { time.Local = saved })
	at := time.Now().In(time.Local).Add(d).Truncate(time.Minute)
	t.Setenv("HRN_CLI_HELPER", "claudelimit")
	t.Setenv("HRN_CLI_LIMIT_MSG", "You've hit your session limit · resets "+at.Format("3:04pm"))
	return at
}

func TestClaudeReportsItsUsageLimitAsLimited(t *testing.T) {
	at := claudeLimitIn(t, 2*time.Hour)
	b := helperCLI(parseClaudeStream)
	b.limits = true
	res := b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Mode: ModeAuto}}, func(Event) {})
	if res.Status != StatusLimited || !res.LimitResets.Equal(at) {
		t.Fatalf("want limited, resetting at %v: status %s, resets %v, error %q", at, res.Status, res.LimitResets, res.Error)
	}
}

func TestOnlyTheClaudeAndCodexAdaptersReadLimits(t *testing.T) {
	for _, c := range []struct {
		b    *CLIBackend
		want bool
	}{{NewClaudeBackend(), true}, {NewCodexBackend(), true}, {NewGeminiBackend(), false}, {NewOpencodeBackend(), false}} {
		if c.b.limits != c.want {
			t.Errorf("%s reads limits = %v, want %v", c.b.name, c.b.limits, c.want)
		}
	}
}

func TestAnAdapterWithoutLimitWordsKeepsTheError(t *testing.T) {
	claudeLimitIn(t, 2*time.Hour)
	res := helperCLI(parseClaudeStream).Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Mode: ModeAuto}}, func(Event) {})
	if res.Status != StatusError {
		t.Fatalf("status = %s: only claude and codex are read for limits", res.Status)
	}
}
