package main

import (
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
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
