package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func recordRun(t *testing.T, events ...agent.Event) string {
	t.Helper()
	w, err := runlog.Create("claude", agent.Task{Prompt: "go", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		w.Event(e)
	}
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	return w.ID()
}

func TestShowUsagePrintsATurnTable(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	id := recordRun(t,
		agent.Event{Kind: agent.EventUsage, Usage: &agent.Usage{Model: "claude-opus-5", InputTokens: 3, OutputTokens: 8, CacheWrite: 12000, ContextTokens: 12003}},
		agent.Event{Kind: agent.EventToolCall, Tool: "Read", Text: "a.go"},
		agent.Event{Kind: agent.EventUsage, Usage: &agent.Usage{Model: "claude-opus-5", InputTokens: 5, OutputTokens: 40, CacheRead: 12000, CacheWrite: 30000, ContextTokens: 42005}},
		agent.Event{Kind: agent.EventCompaction, Text: "auto", Usage: &agent.Usage{ContextTokens: 42045}},
		agent.Event{Kind: agent.EventUsage, Usage: &agent.Usage{Model: "claude-opus-5", InputTokens: 4, OutputTokens: 6, CacheWrite: 9000, ContextTokens: 9004}},
	)
	out, err := captureStdout(t, func() error { return cmdShow([]string{"--usage", id}) })
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("output =\n%s", out)
	}
	if f := strings.Fields(lines[0]); len(f) == 0 || f[0] != "turn" || !strings.Contains(lines[0], "context") || !strings.Contains(lines[0], "cache read so far") {
		t.Errorf("header = %q", lines[0])
	}
	for i, want := range [][]string{
		{"1", "claude-opus-5", "3", "8", "0", "12000", "12003", "0"},
		{"2", "claude-opus-5", "5", "40", "12000", "30000", "42005", "12000"},
		{"3", "claude-opus-5", "4", "6", "0", "9000", "9004", "12000", "compacted", "(auto)", "from", "42045"},
	} {
		f := strings.Fields(lines[1+i])
		if len(f) < 2 {
			t.Fatalf("row %d = %q", i+1, lines[1+i])
		}
		f = append(f[:1], f[2:]...) // the clock depends on the zone
		if strings.Join(f, " ") != strings.Join(want, " ") {
			t.Errorf("row %d = %q, want %q", i+1, lines[1+i], strings.Join(want, " "))
		}
	}
	if lines[4] != "3 turns · median context 12003 · peak 42005 · 12000 cache read" {
		t.Errorf("summary = %q", lines[4])
	}

	out, err = captureStdout(t, func() error { return cmdShow([]string{"--usage", "--json", id}) })
	if err != nil {
		t.Fatal(err)
	}
	var turns []runlog.Turn
	if err := json.Unmarshal(out, &turns); err != nil || len(turns) != 3 || turns[2].Compaction != runlog.CompactionRecorded {
		t.Errorf("json = %s (%v)", out, err)
	}
}

func TestShowUsageSaysWhenARunRecordedNoTurns(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	id := recordRun(t, agent.Event{Kind: agent.EventText, Text: "done"})
	out, err := captureStdout(t, func() error { return cmdShow([]string{"--usage", id}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "no per-turn usage") {
		t.Errorf("output = %q", out)
	}
	if err := cmdShow([]string{"--usage", "--follow", id}); err == nil {
		t.Error("--usage with --follow must be refused")
	}
}
