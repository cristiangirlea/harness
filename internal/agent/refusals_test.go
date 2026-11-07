package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cristiangirlea/harness/internal/kernel"
	"github.com/cristiangirlea/harness/internal/retrieval"
	"github.com/cristiangirlea/harness/internal/tools"
)

type refusalBackend struct{ emitting }

func (refusalBackend) Name() string { return "api" }

func refusalCall(tool string, input map[string]string) Event {
	b, _ := json.Marshal(input)
	return Event{Kind: EventToolCall, Tool: tool, Input: b}
}

func refusalSequence(permission bool) []Event {
	text := tools.GuardError("git-push").Error()
	if permission {
		text = "Permission denied by the user's harness settings."
	}
	return []Event{
		refusalCall("run_command", map[string]string{"command": "TOKEN=value git push origin main"}),
		{Kind: EventToolResult, Tool: "run_command", Text: text, IsError: true},
	}
}

func TestRefusedActionsReachedThroughScriptsWritesAndWrappersAreFlagged(t *testing.T) {
	for _, permission := range []bool{false, true} {
		for _, kind := range []string{"script", "written", "wrapped"} {
			t.Run(kind+map[bool]string{true: "Permission", false: "Guard"}[permission], func(t *testing.T) {
				events := refusalSequence(permission)
				switch kind {
				case "script":
					events = append(events, refusalCall("edit_file", map[string]string{"path": "scripts/send.sh", "new_string": "git push origin main"}), refusalCall("run_command", map[string]string{"command": "bash send.sh"}))
				case "written":
					events = append(events, refusalCall("write_file", map[string]string{"path": "send.sh", "content": "TOKEN=value git\n  push origin main"}))
				case "wrapped":
					events = append(events, refusalCall("run_command", map[string]string{"command": `python -c "import os; os.system('git push origin main')"`}))
				}
				flags := RefusalFlags(events)
				for _, f := range flags {
					if f.Kind == kind {
						wantRule := "git-push"
						if permission {
							wantRule = "permission"
						}
						calls := 0
						for _, e := range events {
							if e.Kind == EventToolCall && !e.Partial {
								calls++
							}
						}
						if f.Rule != wantRule || f.RefusedAt != 0 || f.LaterAt != calls-1 {
							t.Fatalf("flag = %+v", f)
						}
						return
					}
				}
				t.Fatalf("no %s flag: %+v", kind, flags)
			})
		}
	}
}

func TestUnrelatedWorkAndRefusedRetriesHaveNoRefusalFlags(t *testing.T) {
	for _, permission := range []bool{false, true} {
		events := refusalSequence(permission)
		events = append(events,
			refusalCall("read_file", map[string]string{"path": "main.go"}),
			refusalCall("edit_file", map[string]string{"path": "main.go", "new_string": "package main"}),
			refusalCall("run_command", map[string]string{"command": "go test ./..."}),
			refusalCall("run_command", map[string]string{"command": `sh -c "go test ./..."`}),
		)
		events = append(events, refusalSequence(permission)...)
		if got := RefusalFlags(events); len(got) != 0 {
			t.Fatalf("flags = %+v", got)
		}
	}
}

func TestRefusalFlagsAreCappedAndSummariesFitTwoHundredBytes(t *testing.T) {
	events := refusalSequence(false)
	events[0].Text = strings.Repeat("x", 300)
	for range 30 {
		e := refusalCall("run_command", map[string]string{"command": "git push origin main"})
		e.Text = strings.Repeat("y", 300)
		events = append(events, e)
	}
	flags := RefusalFlags(events)
	if len(flags) != 20 {
		t.Fatalf("flags = %d", len(flags))
	}
	for _, f := range flags {
		if len(f.Refused) != 200 || len(f.Later) != 200 {
			t.Fatalf("summary lengths = %d, %d", len(f.Refused), len(f.Later))
		}
	}
}

func TestRunStoresRefusalFlagsWithoutChangingStatusOrCallOrdinals(t *testing.T) {
	events := append([]Event{{Kind: EventText, Text: "starting"}}, refusalSequence(false)...)
	events = append(events, refusalCall("run_command", map[string]string{"command": "git push elsewhere"}))
	res := Run(context.Background(), refusalBackend{emitting{events}}, Task{}, func(Event) {})
	if res.Status != StatusOK || len(res.RefusalFlags) != 1 || res.RefusalFlags[0].RefusedAt != 0 || res.RefusalFlags[0].LaterAt != 1 {
		t.Fatalf("result = %+v", res)
	}
}

func TestRunRecordsToolStatsAlongsideRefusalFlags(t *testing.T) {
	events := append(refusalSequence(false), refusalCall("run_command", map[string]string{"command": "git push elsewhere"}))
	res := Run(context.Background(), refusalBackend{emitting{events}}, Task{}, func(Event) {})
	want := kernel.ToolStat{Calls: 2, Errors: 1, ResultBytes: len(events[1].Text)}
	if len(res.RefusalFlags) != 1 || len(res.ToolStats) != 1 || res.ToolStats["run_command"] != want {
		t.Fatalf("flags = %+v, stats = %+v", res.RefusalFlags, res.ToolStats)
	}
}

func TestEverySupportedWrapperFlagsTheRefusedProgramAndSubcommand(t *testing.T) {
	for _, wrapper := range []string{"sh -c", "bash -c", "pwsh -Command", "powershell -Command", "cmd /c", "python -c", "python3 -c", "node -e", "perl -e", "ruby -e"} {
		t.Run(wrapper, func(t *testing.T) {
			for _, permission := range []bool{false, true} {
				events := append(refusalSequence(permission), refusalCall("run_command", map[string]string{"command": wrapper + ` "git push origin main"`}))
				found := false
				for _, f := range RefusalFlags(events) {
					found = found || f.Kind == "wrapped"
				}
				if !found {
					t.Fatal("missing wrapped flag")
				}
			}
		})
	}
}

func TestRefusalsPairWithTheLatestUnansweredCallOfTheSameTool(t *testing.T) {
	events := []Event{
		refusalCall("run_command", map[string]string{"command": "go test ./..."}),
		refusalCall("run_command", map[string]string{"command": "git push origin main"}),
		{Kind: EventToolCall, Tool: "run_command", Partial: true},
		{Kind: EventToolResult, Tool: "read_file", IsError: true, Text: tools.GuardError("sudo").Error()},
		{Kind: EventToolResult, Tool: "run_command", IsError: true, Text: tools.GuardError("git-push").Error()},
		{Kind: EventToolResult, Tool: "run_command", Text: "ok"},
		refusalCall("run_command", map[string]string{"command": "git push elsewhere"}),
	}
	flags := RefusalFlags(events)
	if len(flags) != 1 || flags[0].RefusedAt != 1 || flags[0].LaterAt != 2 {
		t.Fatalf("flags = %+v", flags)
	}
}

func TestScriptFlagsUseOnlyEarlierWritesAndTheSameRule(t *testing.T) {
	for _, permission := range []bool{false, true} {
		for _, tool := range []string{"write_file", "edit_file"} {
			events := []Event{refusalCall(tool, map[string]string{"path": `scripts\send.sh`, "content": "git push", "new_string": "git push"})}
			events = append(events, refusalSequence(permission)...)
			events = append(events, refusalCall("run_command", map[string]string{"command": "bash scripts/send.sh"}))
			flags := RefusalFlags(events)
			if len(flags) != 1 || flags[0].Kind != "script" || flags[0].LaterAt != 2 {
				t.Fatalf("flags = %+v", flags)
			}
		}
	}
	for _, command := range []string{"bash other.sh", "sudo go test", "bash send.sh.bak"} {
		events := append(refusalSequence(false), refusalCall("run_command", map[string]string{"command": command}), refusalCall("write_file", map[string]string{"path": "send.sh", "content": "git push"}))
		if got := RefusalFlags(events); len(got) != 0 {
			t.Fatalf("%s: flags = %+v", command, got)
		}
	}
}

func TestOrdinaryErrorsAndShortRefusedCommandsDoNotFlagWrites(t *testing.T) {
	for _, text := range []string{"command failed", tools.GuardError("git-push").Error()} {
		events := refusalSequence(false)
		events[1].Text, events[1].IsError = text, false
		events = append(events, refusalCall("run_command", map[string]string{"command": "git push"}))
		if got := RefusalFlags(events); len(got) != 0 {
			t.Fatal(got)
		}
	}
	events := refusalSequence(true)
	events[0] = refusalCall("run_command", map[string]string{"command": "go env"})
	events = append(events, refusalCall("write_file", map[string]string{"content": "go env", "path": "env.sh"}))
	if got := RefusalFlags(events); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestRefusalSummariesRemainValidUTF8(t *testing.T) {
	events := refusalSequence(false)
	events[0].Text = strings.Repeat("a", 199) + "\u20ac"
	events = append(events, refusalCall("run_command", map[string]string{"command": "git push"}))
	flags := RefusalFlags(events)
	if len(flags) != 1 || len(flags[0].Refused) > 200 || !utf8.ValidString(flags[0].Refused) {
		t.Fatal(flags)
	}
}

func TestDurableRunsDoNotComputeRefusalFlagsFromEvents(t *testing.T) {
	events := append(refusalSequence(false), refusalCall("run_command", map[string]string{"command": "git push"}))
	res := Run(context.Background(), refusalBackend{emitting{events}}, Task{Durable: &kernel.DurableExecution{}}, func(Event) {})
	if len(res.RefusalFlags) != 0 || res.ToolCalls != 99 {
		t.Fatalf("result = %+v", res)
	}
}

func TestRunRefusalIndexesCountOnlyToolCallsInEachInvocation(t *testing.T) {
	for _, partial := range []bool{false, true} {
		events := []Event{{Kind: EventText, Text: "start"}}
		if partial {
			events = append([]Event{{Kind: EventText, Text: "fragment", Partial: true}}, events...)
		}
		events = append(events, refusalSequence(false)...)
		events = append(events, refusalCall("run_command", map[string]string{"command": "git push"}))
		task := Task{RunDir: t.TempDir(), RetrievalState: &retrieval.Record{
			Mode: "optional", Scope: retrieval.Scope{Backend: "api"}, Failure: "optional retrieval unavailable",
		}}
		var recorded = []Event{{Kind: EventInfo, Text: "caller event"}}
		for range 2 {
			task.RunDir = t.TempDir()
			res := Run(context.Background(), refusalBackend{emitting{events}}, task, func(e Event) {
				if !e.Partial {
					recorded = append(recorded, e)
				}
			})
			if res.Status != StatusOK || len(res.RefusalFlags) != 1 {
				t.Fatalf("result = %+v", res)
			}
			f := res.RefusalFlags[0]
			if f.RefusedAt != 0 || f.LaterAt != 1 {
				t.Fatalf("flag = %+v, events = %+v", f, recorded)
			}
		}
	}
}

func TestWrittenFilesAreFlaggedOnlyWhenExecuted(t *testing.T) {
	for _, permission := range []bool{false, true} {
		for _, tc := range []struct {
			command string
			want    bool
		}{
			{"cat send.sh", false}, {"git add send.sh", false}, {"git diff send.sh", false}, {"gofmt send.sh", false},
			{"bash send.sh", true}, {"./send.sh", true}, {"scripts/send.sh", true}, {"send.sh", true}, {"source send.sh", true}, {". send.sh", true}, {"python3 tool.py", true},
			{"cat send.sh; bash send.sh", true}, {"go test && ./send.sh", true}, {"go test || source send.sh", true}, {"cat input | python3 tool.py", true},
			{`bash -c "cat send.sh"`, false}, {`python -c "print('send.sh')"`, false}, {`node -e "console.log('send.sh')"`, false}, {`pwsh -Command "Get-Content send.sh"`, false},
			{"bash -x send.sh", true}, {`bash "scripts/send.sh"`, true},
			{`cat 'note; ./send.sh'`, false}, {`cat "note && ./send.sh"`, false},
			{`bash -c "cat x; ./send.sh"`, false}, {`node -e "false || ./send.sh"`, false},
			{"scripts/bash", true}, {"./python3", true},
		} {
			t.Run(fmt.Sprintf("%t/%s", permission, tc.command), func(t *testing.T) {
				events := refusalSequence(permission)
				filenames := []string{"scripts/send.sh", "tool.py"}
				if tc.command == "scripts/bash" {
					filenames = append(filenames, "scripts/bash")
				}
				if tc.command == "./python3" {
					filenames = append(filenames, "python3")
				}
				for _, filename := range filenames {
					events = append(events, refusalCall("write_file", map[string]string{"path": filename, "content": "git push origin main"}))
				}
				events = append(events, refusalCall("run_command", map[string]string{"command": tc.command}))
				got := RefusalFlags(events)
				if (len(got) > 0) != tc.want {
					t.Fatalf("flags = %+v, want flag %t", got, tc.want)
				}
			})
		}
	}
}

func TestRefusalAnalysisBoundsRepeatedAndDistinctRefusals(t *testing.T) {
	for _, distinct := range []bool{false, true} {
		var events []Event
		for i := range 200 {
			command := "git push origin main"
			if distinct {
				command = fmt.Sprintf("git push origin branch%d", i)
			}
			events = append(events, refusalCall("run_command", map[string]string{"command": command}), Event{Kind: EventToolResult, Tool: "run_command", IsError: true, Text: tools.GuardError("git-push").Error()})
		}
		for i := range 500 {
			events = append(events, refusalCall("write_file", map[string]string{"path": fmt.Sprintf("file%d.sh", i), "content": strings.Repeat("echo unrelated\n", 20)}))
		}
		for range 500 {
			events = append(events, refusalCall("run_command", map[string]string{"command": "bash file0.sh"}))
		}
		start := time.Now()
		flags := RefusalFlags(events)
		elapsed := time.Since(start)
		t.Logf("distinct=%t: analysis took %s", distinct, elapsed)
		if elapsed >= 5*time.Second {
			t.Fatalf("distinct=%t: analysis took %s", distinct, elapsed)
		}
		if len(flags) != 0 {
			t.Fatalf("unrelated flags = %+v", flags)
		}
		events = append(events, refusalCall("run_command", map[string]string{"command": "git push origin other"}))
		flags = RefusalFlags(events)
		want := 1
		if distinct {
			want = 20
		}
		if len(flags) != want {
			t.Fatalf("distinct=%t: flags = %d, want %d", distinct, len(flags), want)
		}
	}
}

func TestClaudeShapedRefusalsDoNotProduceFlags(t *testing.T) {
	events := []Event{
		refusalCall("Bash", map[string]string{"command": "git push origin main"}),
		{Kind: EventToolResult, IsError: true, Text: tools.GuardError("git-push").Error()},
		refusalCall("Write", map[string]string{"file_path": "send.sh", "content": "git push origin main"}),
		refusalCall("Bash", map[string]string{"command": "bash send.sh"}),
	}
	if flags := RefusalFlags(events); len(flags) != 0 {
		t.Fatal(flags)
	}
	apiEvents := append(refusalSequence(false), refusalCall("run_command", map[string]string{"command": "git push"}))
	res := Run(context.Background(), emitting{apiEvents}, Task{}, func(Event) {})
	if len(res.RefusalFlags) != 0 {
		t.Fatal(res.RefusalFlags)
	}
}

func TestQuotedEnvironmentAssignmentsDoNotHideTheRefusedProgram(t *testing.T) {
	events := refusalSequence(true)
	events[0] = refusalCall("run_command", map[string]string{"command": `FOO="two words" BAR=value git push origin main`})
	events = append(events, refusalCall("run_command", map[string]string{"command": `bash -c "git push elsewhere"`}))
	for _, f := range RefusalFlags(events) {
		if f.Kind == "wrapped" {
			return
		}
	}
	t.Fatal("missing wrapped flag")
}

func TestRefusalSummariesMaskSecretsBeforeTheCut(t *testing.T) {
	token := "glpat-" + strings.Repeat("Zq7", 100)
	events := []Event{
		refusalCall("run_command", map[string]string{"command": "git push https://oauth2:" + token + "@gitlab.com/group/repo.git main"}),
		{Kind: EventToolResult, Tool: "run_command", Text: tools.GuardError("git-push").Error(), IsError: true},
		refusalCall("run_command", map[string]string{"command": "git push https://oauth2:" + token + "@gitlab.com/group/repo.git main"}),
	}
	flags := RefusalFlags(events)
	if len(flags) == 0 {
		t.Fatal("no flags")
	}
	for _, f := range flags {
		for _, s := range []string{f.Refused, f.Later} {
			if strings.Contains(s, "glpat") || strings.Contains(s, "Zq7") || len(s) > 200 {
				t.Fatalf("summary = %q", s)
			}
		}
	}
}

func TestRefusalAnalysisStaysLinearInManyPathsAndLongCommands(t *testing.T) {
	events := refusalSequence(false)
	// Distinct operands: each one is matched against every written file unless the lookup is by key.
	var distinctOperands strings.Builder
	for i := range 20000 {
		fmt.Fprintf(&distinctOperands, "./o%d.sh;", i)
	}
	for i := range 5000 {
		content := fmt.Sprintf("echo %d", i)
		if i == 42 {
			content = "git push origin main"
		}
		events = append(events, refusalCall("write_file", map[string]string{"path": fmt.Sprintf("d%d/s%d.sh", i, i), "content": content}))
	}
	events = append(events,
		refusalCall("run_command", map[string]string{"command": distinctOperands.String() + "./s42.sh"}),
		refusalCall("run_command", map[string]string{"command": strings.Repeat("sh -c ", 17000) + "git push origin main"}),
	)
	start := time.Now()
	flags := RefusalFlags(events)
	// Linear work takes about 3s on hosted runners and quadratic work about 45s on a desktop; the race detector hides the difference.
	if elapsed := time.Since(start); elapsed >= 10*time.Second && !raceEnabled {
		t.Fatalf("analysis took %s", elapsed)
	}
	var got []string
	for _, f := range flags {
		got = append(got, fmt.Sprintf("%s@%d", f.Kind, f.LaterAt))
	}
	if want := "script@5001 script@5002 wrapped@5002"; strings.Join(got, " ") != want {
		t.Fatalf("flags = %v, want %s", got, want)
	}
}
