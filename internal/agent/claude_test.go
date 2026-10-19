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
	if source == "second" {
		fmt.Println(`{"type":"system","subtype":"init","apiKeySource":"none"}`)
		source = "apiKeyHelper"
	}
	init := `{"type":"system","subtype":"init"}`
	if source != "missing" {
		init = fmt.Sprintf(`{"type":"system","subtype":"init","apiKeySource":%q}`, source)
	}
	fmt.Println(init)
	if source != "none" && source != "missing" {
		time.Sleep(5 * time.Second)
		fmt.Println(`{"type":"assistant","message":{"content":[{"type":"text","text":"paid turn"}]}}`)
	}
	fmt.Println(`{"type":"result","subtype":"success","result":"done"}`)
	os.Exit(0)
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
				if res.Status != StatusError || !strings.Contains(res.Error, tc.source) || !strings.Contains(res.Error, "refused") {
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
	opts := Options{Mode: ModeAuto, MCP: []string{"a.json", "b.json"}}
	plain := NewClaudeBackend().args(Task{Options: opts}, "")
	opts.Lean = Bool(true)
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
	plain := NewClaudeBackend().args(Task{Options: Options{Mode: ModeAuto}}, "")
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
