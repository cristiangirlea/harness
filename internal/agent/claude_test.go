package agent

import (
	"context"
	"fmt"
	"os"
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
			res := b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Timeout: Duration(500 * time.Millisecond)}},
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
