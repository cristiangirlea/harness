package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestHelperClaudeKeySource acts as Claude Code without starting a paid turn.
func TestHelperClaudeKeySource(t *testing.T) {
	source := os.Getenv("HRN_CLAUDE_KEY_SOURCE_TEST")
	if source == "" {
		return
	}
	if source == "empty" {
		os.Exit(0)
	}
	if source == "crash" {
		fmt.Fprintln(os.Stderr, "boom: settings could not be read")
		os.Exit(3)
	}
	if source == "helper-then-none" {
		fmt.Println(`{"type":"system","subtype":"init","apiKeySource":"apiKeyHelper"}`)
		source = "wait-none"
	}
	if source == "second" {
		fmt.Println(`{"type":"system","subtype":"init","apiKeySource":"none"}`)
		source = "apiKeyHelper"
	}
	const answer = `{"type":"assistant","message":{"content":[{"type":"text","text":"answer"}]}}`
	block := source != "none" && source != "missing"
	switch source {
	case "wait-none":
		source, block = "none", true
	case "fast-helper":
		source, block = "apiKeyHelper", false
	}
	switch source {
	case "assistant", "wait-assistant":
		fmt.Println(answer)
		block = source == "wait-assistant"
	case "missing":
		fmt.Println(`{"type":"system","subtype":"init"}`)
	default:
		fmt.Printf("{\"type\":\"system\",\"subtype\":\"init\",\"apiKeySource\":%q}\n", source)
		if source == "apiKeyHelper" && !block {
			fmt.Println(answer)
		}
	}
	if block {
		time.Sleep(5 * time.Second)
		fmt.Println(`{"type":"assistant","message":{"content":[{"type":"text","text":"paid turn"}]}}`)
	}
	fmt.Println(`{"type":"result","subtype":"success","result":"done"}`)
	os.Exit(0)
}

func TestClaudeProviderChecksInitKeySource(t *testing.T) {
	const refusal = "provider gateway: the claude CLI has no key helper configured, so it would send its plan login to the gateway; set apiKeyHelper in the CLI's settings"
	for _, tc := range []struct {
		target, source string
		refuse         bool
		wantText       []string
	}{
		{"gateway", "wait-none", true, nil},
		{"gateway", "missing", true, nil},
		{"gateway", "ANTHROPIC_API_KEY", true, nil},
		{"gateway", "oauth", true, nil},
		{"gateway", "unknown", true, nil},
		{"gateway", "wait-assistant", true, nil},
		{"gateway", "empty", true, nil},
		{"gateway", "helper-then-none", true, nil},
		{"gateway", "fast-helper", false, []string{"answer"}},
		{"bedrock", "none", false, nil},
		{"bedrock", "assistant", false, []string{"answer"}},
	} {
		t.Run(tc.target+"/"+tc.source, func(t *testing.T) {
			t.Setenv("HRN_CLAUDE_KEY_SOURCE_TEST", tc.source)
			b := NewClaudeBackend()
			b.ProviderTarget = tc.target
			b.candidates = func() []string { return []string{os.Args[0]} }
			t.Setenv("PATH", t.TempDir())
			b.args = func(Task, string) []string { return []string{"-test.run=^TestHelperClaudeKeySource$", "--"} }
			task := Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Timeout: Duration(2 * time.Second)}}
			var text []string
			res := b.Run(context.Background(), task, func(e Event) {
				if e.Kind == EventText {
					text = append(text, e.Text)
				}
			})
			if tc.refuse {
				if res.Status != StatusError || res.Error != refusal || len(text) != 0 || res.Final != "" || !res.StartRefusal {
					t.Fatalf("want the CLI run refused before using output: status %s, error %q, text %q, final %q", res.Status, res.Error, text, res.Final)
				}
			} else if res.Status != StatusOK || res.Final != "done" || !slices.Equal(text, tc.wantText) || res.StartRefusal {
				t.Fatalf("want the run to finish with text %q: status %s, error %q, text %q, final %q", tc.wantText, res.Status, res.Error, text, res.Final)
			}
		})
	}
}

func TestClaudeGatewayReportsTheCLIsOwnErrorBeforeTheRefusal(t *testing.T) {
	t.Setenv("HRN_CLAUDE_KEY_SOURCE_TEST", "crash")
	b := NewClaudeBackend()
	b.ProviderTarget = "gateway"
	b.candidates = func() []string { return []string{os.Args[0]} }
	t.Setenv("PATH", t.TempDir())
	b.args = func(Task, string) []string { return []string{"-test.run=^TestHelperClaudeKeySource$", "--"} }
	res := b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Timeout: Duration(20 * time.Second)}}, func(Event) {})
	if res.Status != StatusError || !strings.Contains(res.Error, "boom: settings could not be read") {
		t.Fatalf("want the CLI's own error: status %s, error %q", res.Status, res.Error)
	}
}

func TestClaudeRefusesPerTokenBillingAtInit(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		refuse       bool
	}{
		{"plan login", "none", false},
		{"environment key", "ANTHROPIC_API_KEY", true},
		{"key helper", "apiKeyHelper", true},
		{"older CLI", "missing", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HRN_CLAUDE_KEY_SOURCE_TEST", tc.source)
			b := NewClaudeBackend()
			b.name = os.Args[0]
			b.candidates = nil
			b.args = func(Task, string) []string { return []string{"-test.run=^TestHelperClaudeKeySource$", "--"} }
			var text []string
			// One deadline for every case, long enough for a busy PC to start the
			// helper: a refusal that did not stop the CLI still ends as a timeout,
			// since the helper sleeps 5s after init, and that fails the test below.
			res := b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Timeout: Duration(2 * time.Second)}},
				func(e Event) {
					if e.Kind == EventText {
						text = append(text, e.Text)
					}
				})
			if tc.refuse {
				if res.Status != StatusError || !strings.Contains(res.Error, tc.source) || !strings.Contains(res.Error, "refused") || !res.StartRefusal {
					t.Fatalf("key source %q: want billing refusal, got %+v", tc.source, res)
				}
				if len(text) != 0 || res.Final != "" {
					t.Fatalf("key source %q: used output after init: text %v, result %+v", tc.source, text, res)
				}
			} else if res.Status != StatusOK || res.Final != "done" {
				t.Fatalf("key source %q: plan login should run: %+v", tc.source, res)
			}
		})
	}
}

func TestClaudeRefusesPerTokenBillingAtSecondInit(t *testing.T) {
	t.Setenv("HRN_CLAUDE_KEY_SOURCE_TEST", "second")
	b := NewClaudeBackend()
	b.name = os.Args[0]
	b.candidates = nil
	b.args = func(Task, string) []string { return []string{"-test.run=^TestHelperClaudeKeySource$", "--"} }
	var text []string
	res := b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Timeout: Duration(2 * time.Second)}},
		func(e Event) {
			if e.Kind == EventText {
				text = append(text, e.Text)
			}
		})
	if res.Status != StatusError || !strings.Contains(res.Error, "apiKeyHelper") || !strings.Contains(res.Error, "refused") {
		t.Fatalf("second init: want billing refusal, got %+v", res)
	}
	if len(text) != 0 || res.Final != "" {
		t.Fatalf("second init: used output after init: text %v, result %+v", text, res)
	}
}

func TestHelperClaudeResultError(t *testing.T) {
	marker := os.Getenv("HRN_CLAUDE_ERROR_TEST")
	if marker == "" {
		return
	}
	fmt.Println(`{"type":"result","subtype":"success","is_error":true,"result":"Not logged in"}`)
	fmt.Fprintln(os.Stderr, "CLI stderr: login failed")
	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(marker, []byte("exited on its own"), 0600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	os.Exit(2)
}

func TestClaudeResultErrorPreservesCLIExit(t *testing.T) {
	marker := t.TempDir() + string(os.PathSeparator) + "exited"
	t.Setenv("HRN_CLAUDE_ERROR_TEST", marker)
	b := NewClaudeBackend()
	b.name = os.Args[0]
	b.candidates = nil
	b.args = func(Task, string) []string { return []string{"-test.run=^TestHelperClaudeResultError$", "--"} }
	res := b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Timeout: Duration(2 * time.Second)}}, func(Event) {})
	if res.Status != StatusError || res.Error != "CLI stderr: login failed" {
		t.Fatalf("want CLI stderr on normal exit, got %+v", res)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("CLI did not reach its own exit: %v", err)
	}
}

func TestClaudePassesEffortOnlyWhenSet(t *testing.T) {
	for _, effort := range []string{"", "low", "medium", "high", "xhigh", "max"} {
		t.Run("effort="+effort, func(t *testing.T) {
			args := NewClaudeBackend().args(Task{Options: Options{Effort: effort}}, "")
			index := slices.Index(args, "--effort")
			if effort == "" {
				if index != -1 {
					t.Errorf("unset effort must keep the CLI default: %v", args)
				}
				return
			}
			if index < 0 || index+1 >= len(args) || args[index+1] != effort {
				t.Errorf("want --effort %s: %v", effort, args)
			}
		})
	}
}

func TestClaudeDisablesToolsOnlyWhenRequested(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		task := Task{NoTools: disabled, Options: Options{Lean: Bool(true)}}
		args := NewClaudeBackend().args(task, "")
		i := slices.Index(args, "--tools")
		if disabled {
			if i < 0 || i+1 >= len(args) || args[i+1] != "" || !slices.Contains(args, "--strict-mcp-config") || slices.Contains(args, "--mcp-config") {
				t.Errorf("unsafe feed args: %q", args)
			}
		} else if i >= 0 {
			t.Errorf("ordinary run changed: %q", args)
		}
	}
}

func TestClaudePassesCompactWindowOnlyWhenSet(t *testing.T) {
	for _, window := range []CompactWindow{"", "auto", "200000"} {
		args := NewClaudeBackend().args(Task{Options: Options{CompactWindow: window}}, "")
		i := slices.Index(args, "--autocompact")
		if window == "" {
			if i != -1 {
				t.Errorf("unset: Claude Code's own threshold must stay: %v", args)
			}
			continue
		}
		if i < 0 || i+1 >= len(args) || args[i+1] != string(window) {
			t.Errorf("want --autocompact %s: %v", window, args)
		}
		if d := slices.Index(args, "--disallowedTools"); d < i {
			t.Errorf("--autocompact must come before the deny rules: %v", args)
		}
	}
}

func TestClaudeLeanAddsOnlyStrictMCPConfig(t *testing.T) {
	opts := Options{Mode: ModeAuto, MCP: []string{"a.json", "b.json"}, Lean: Bool(false)}
	plain := NewClaudeBackend().args(Task{Options: opts}, "")
	opts.Lean, opts.Skills = Bool(true), Bool(true)
	lean := NewClaudeBackend().args(Task{Options: opts}, "")
	for _, flag := range []string{"--strict-mcp-config", "--setting-sources", "--bare"} {
		if slices.Contains(plain, flag) {
			t.Errorf("without lean, %s must not be passed: %v", flag, plain)
		}
	}
	if slices.Contains(lean, "--bare") {
		t.Errorf("--bare ignores the subscription login: %v", lean)
	}
	// Exactly this one flag, before the deny rules: --disallowedTools takes
	// every word up to the next flag, so the guard's rules stay as they were.
	// No --setting-sources: the user's rules, plugins and skills still load.
	d := slices.Index(plain, "--disallowedTools")
	if d < 0 {
		t.Fatalf("the guard's deny rules are missing: %v", plain)
	}
	want := slices.Insert(slices.Clone(plain), d, "--strict-mcp-config")
	if !slices.Equal(lean, want) {
		t.Errorf("lean args:\n got %v\nwant %v", lean, want)
	}
}

func TestClaudeAppendsTheSystemPromptAsOneArgumentAndCanTurnSkillsOff(t *testing.T) {
	plain := NewClaudeBackend().args(Task{Options: Options{Mode: ModeAuto, Lean: Bool(false)}}, "")
	for _, flag := range []string{"--disable-slash-commands", appendSystemPromptFlag} {
		if slices.ContainsFunc(plain, func(a string) bool { return strings.HasPrefix(a, flag) }) {
			t.Errorf("unset, %s must not be passed: %v", flag, plain)
		}
	}
	text := "-not a flag\nsecond line"
	args := NewClaudeBackend().args(Task{Options: Options{Mode: ModeAuto, AppendSystemPrompt: text, NoSkills: true}}, "")
	i := slices.Index(args, appendSystemPromptFlag+"="+text)
	s := slices.Index(args, "--disable-slash-commands")
	d := slices.Index(args, "--disallowedTools")
	if i < 0 || s < 0 || d < i || d < s {
		t.Errorf("want the text in one argument and the skills switch, both before the deny rules: %q", args)
	}
	if shown := strings.Join(shownArgs(args), " "); strings.Contains(shown, "second line") || !strings.Contains(shown, appendSystemPromptFlag+"=<23 bytes>") {
		t.Errorf("the run's info line names the size, not the text: %s", shown)
	}
}

func TestClaudeRefusesASystemPromptThroughABatchFileShim(t *testing.T) {
	shim := filepath.Join(t.TempDir(), "claude.cmd")
	if err := os.WriteFile(shim, []byte("@echo off\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	b := NewClaudeBackend()
	b.name = "hrn-no-such-claude"
	b.candidates = func() []string { return []string{shim} }
	res := b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: Options{AppendSystemPrompt: "a & b"}}, func(Event) {})
	if res.Status != StatusError || !strings.Contains(res.Error, "cannot be passed safely through claude.cmd") {
		t.Errorf("res = %+v", res)
	}
}
