package agent

import (
	"slices"
	"testing"
)

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

func TestClaudeLeanAddsOnlyTheStrictFlags(t *testing.T) {
	opts := Options{Mode: ModeAuto, MCP: []string{"a.json", "b.json"}}
	plain := NewClaudeBackend().args(Task{Options: opts}, "")
	opts.Lean = true
	lean := NewClaudeBackend().args(Task{Options: opts}, "")
	for _, flag := range []string{"--strict-mcp-config", "--setting-sources", "--bare"} {
		if slices.Contains(plain, flag) {
			t.Errorf("without lean, %s must not be passed: %v", flag, plain)
		}
	}
	if slices.Contains(lean, "--bare") {
		t.Errorf("--bare ignores the subscription login: %v", lean)
	}
	// Exactly these three words, before the deny rules: --disallowedTools takes
	// every word up to the next flag, so the guard's rules stay as they were.
	d := slices.Index(plain, "--disallowedTools")
	if d < 0 {
		t.Fatalf("the guard's deny rules are missing: %v", plain)
	}
	want := slices.Insert(slices.Clone(plain), d, "--strict-mcp-config", "--setting-sources", "project")
	if !slices.Equal(lean, want) {
		t.Errorf("lean args:\n got %v\nwant %v", lean, want)
	}
}
