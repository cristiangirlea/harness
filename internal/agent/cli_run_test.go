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
	case "claudelimit":
		// The test picks the reset time, so the run does not depend on the hour.
		msg, _ := json.Marshal(os.Getenv("HRN_CLI_LIMIT_MSG"))
		fmt.Printf(`{"type":"result","subtype":"success","is_error":true,"num_turns":1,"result":%s}`+"\n", msg)
		os.Exit(1)
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

func TestCLISaysTheGuardDoesNotApply(t *testing.T) {
	t.Setenv("HRN_CLI_HELPER", "error")
	guardLines := func(mode Mode) []string {
		var out []string
		helperCLI(parseOpencodeStream).Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Mode: mode}},
			func(e Event) {
				if e.Kind == EventInfo && strings.Contains(e.Text, "command guard") {
					out = append(out, e.Text)
				}
			})
		return out
	}
	if lines := guardLines(ModeYolo); len(lines) != 1 || !strings.Contains(lines[0], "not applied") {
		t.Errorf("an unattended CLI run should say the guard is not applied: %v", lines)
	}
	// Any other mode still asks or refuses per call, so the line would be noise.
	if lines := guardLines(ModeAuto); len(lines) != 0 {
		t.Errorf("mode auto should not carry the line: %v", lines)
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
