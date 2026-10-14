package main

import (
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// Record text is whatever an agent read, so it reaches the terminal without
// the control characters that would let it set the window title, write the
// clipboard (OSC 52), clear the screen, move the cursor, or forge a ✓ line
// over a ✗ one. Newlines and tabs stay; Windows line endings read as lines.
func TestRecordTextCannotDriveTheTerminal(t *testing.T) {
	hostile := "ok\x1b]0;pwned\x07\x1b]52;c;cm0gLXJmIH4=\x07\x1b[2J\x1b[1A\x1b[2K\x9b31mred\rfake ✓\x00\x7f"
	var b strings.Builder
	r := &renderer{out: &b, thinking: true}
	for _, e := range []agent.Event{
		{Kind: agent.EventText, Text: hostile + "\r\nsecond line\tafter a tab"},
		{Kind: agent.EventThinking, Text: hostile},
		{Kind: agent.EventToolCall, Tool: "Bash\x1b[2J", Text: hostile},
		{Kind: agent.EventToolResult, Text: hostile},
		{Kind: agent.EventInfo, Text: hostile},
		{Kind: agent.EventError, Text: hostile},
	} {
		r.Event(e)
	}
	out := b.String()
	for _, bad := range []string{"\x1b", "\x07", "\x9b", "\x00", "\x7f", "\r"} {
		if strings.Contains(out, bad) {
			t.Errorf("the terminal got %q from record text:\n%q", bad, out)
		}
	}
	if !strings.Contains(out, "\nsecond line\tafter a tab") {
		t.Errorf("a CRLF line ending and a tab should survive as a line and a tab:\n%q", out)
	}
}

// The rest of what the terminal shows of a run, from the backend or the model
// rather than from files: the header, the summary line, and a tool call shown
// by its JSON input.
func TestRunFieldsCannotDriveTheTerminalEither(t *testing.T) {
	var b strings.Builder
	r := &renderer{out: &b}
	rec := runlog.Record{ID: "r1", Backend: "claude\x1b[2J", Continues: "x\x1b]0;t\x07",
		Task:   agent.Task{Prompt: "p", Dir: "/repo\x1b[1A"},
		Result: agent.Result{Session: "s\x9b31m", Model: "m\x1b]52;c;eA==\x07", Backend: "b\x07", Status: agent.StatusOK}}
	showHeader(r, rec)
	r.Event(agent.Event{Kind: agent.EventToolCall, Tool: "mcp", Input: []byte("{\"q\":\"\u009b31m\u007f\"}")})
	r.Summary(rec.ID, rec.Result)
	for _, bad := range []string{"\x1b", "\x07", "\u009b", "\x7f"} {
		if strings.Contains(b.String(), bad) {
			t.Errorf("the terminal got %q:\n%q", bad, b.String())
		}
	}
}

func TestRendererMasksBeforeShortening(t *testing.T) {
	secret := "Authorization: Bearer " + strings.Repeat("z", 300)
	for _, kind := range []agent.EventKind{agent.EventText, agent.EventThinking, agent.EventToolCall, agent.EventToolResult, agent.EventInfo, agent.EventSession, agent.EventError} {
		var b strings.Builder
		r := &renderer{out: &b, thinking: true}
		r.Event(agent.Event{Kind: kind, Text: secret})
		if strings.Contains(b.String(), "zz") || !strings.Contains(b.String(), "••••") {
			t.Errorf("%s leaks: %q", kind, b.String())
		}
	}
	for _, input := range []string{`{"command":"` + secret + `"}`, `{"password":"` + strings.Repeat("z", 300) + `"}`} {
		var b strings.Builder
		r := &renderer{out: &b}
		r.Event(agent.Event{Kind: agent.EventToolCall, Input: []byte(input)})
		if strings.Contains(b.String(), "zz") || !strings.Contains(b.String(), "••••") {
			t.Errorf("input leaks: %q", b.String())
		}
	}
	var b strings.Builder
	r := &renderer{out: &b}
	r.Line(secret)
	showHeader(r, runlog.Record{Task: agent.Task{Prompt: secret}})
	if strings.Contains(b.String(), "zz") || strings.Count(b.String(), "••••") != 2 {
		t.Errorf("lines leak: %q", b.String())
	}
}
