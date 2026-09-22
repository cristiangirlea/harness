package agent

import (
	"context"
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
	res := helperCLI(parseOpencodeStream).Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Mode: ModeAsk}, func(Event) {})
	if res.Status != StatusError || !strings.Contains(res.Error, "usage limit reached") {
		t.Fatalf("the stream's error should survive a non-zero exit: %+v", res)
	}
}

func TestCLIKeepsStreamStatusOnNonZeroExit(t *testing.T) {
	t.Setenv("HRN_CLI_HELPER", "maxturns")
	res := helperCLI(parseClaudeStream).Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Mode: ModeAsk}, func(Event) {})
	if res.Status != StatusMaxTurns || res.Error != "error_max_turns" || res.Turns != 3 {
		t.Fatalf("a status the stream set should survive a non-zero exit: %+v", res)
	}
}

func TestCLISaysWhatItCannotEnforce(t *testing.T) {
	t.Setenv("HRN_CLI_HELPER", "error")
	var infos []string
	b := helperCLI(parseOpencodeStream)
	b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Mode: ModeAsk, MaxCostUSD: 5, TaskBudget: 50000},
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
		helperCLI(parseOpencodeStream).Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Mode: mode},
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
		Task{Prompt: "go", Dir: t.TempDir(), Mode: ModeAsk, Timeout: 200 * time.Millisecond}, func(Event) {})
	if res.Status != StatusTimeout {
		t.Fatalf("status = %s, want timeout (error %q)", res.Status, res.Error)
	}
}
