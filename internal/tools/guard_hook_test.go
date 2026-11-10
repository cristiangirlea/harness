package tools

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func guardHookAnswer(t *testing.T, input string) (string, string, int) {
	t.Helper()
	var out strings.Builder
	code := GuardHook(strings.NewReader(input), &out, Guard)
	if out.Len() == 0 {
		return "", "", code
	}
	var a struct {
		Specific struct {
			Event    string `json:"hookEventName"`
			Decision string `json:"permissionDecision"`
			Reason   string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out.String()), &a); err != nil {
		t.Fatalf("answer %q: %v", out.String(), err)
	}
	if a.Specific.Event != "PreToolUse" {
		t.Errorf("answer %q names another event", out.String())
	}
	return a.Specific.Decision, a.Specific.Reason, code
}

func TestGuardHookRefusesWhatTheGuardRefusesOnTheOriginalText(t *testing.T) {
	for _, tool := range []string{"Bash", "PowerShell"} {
		input, _ := json.Marshal(map[string]any{"tool_name": tool, "tool_input": map[string]string{"command": "gh repo edit a/b --visibility public"}})
		decision, reason, code := guardHookAnswer(t, string(input))
		if decision != "deny" || code != 0 || !strings.HasPrefix(reason, GuardRefusal+" (gh-publish)") {
			t.Errorf("%s: %q %q exit %d", tool, decision, reason, code)
		}
	}
}

func TestGuardHookSaysNothingOfACommandTheGuardAllows(t *testing.T) {
	decision, _, code := guardHookAnswer(t, `{"tool_name":"Bash","tool_input":{"command":"go test ./..."}}`)
	if decision != "" || code != 0 {
		t.Fatalf("%q exit %d", decision, code)
	}
}

func TestGuardHookLeavesOtherToolsAlone(t *testing.T) {
	decision, _, code := guardHookAnswer(t, `{"tool_name":"Read","tool_input":{"file_path":"git push"}}`)
	if decision != "" || code != 0 {
		t.Fatalf("%q exit %d", decision, code)
	}
}

func TestGuardHookRefusesWhatItCannotRead(t *testing.T) {
	for name, input := range map[string]string{
		"not json":   `{`,
		"no command": `{"tool_name":"Bash","tool_input":{}}`,
		"too large":  `{"tool_name":"Bash","x":"` + strings.Repeat("a", guardHookMaxInput) + `"}`,
	} {
		decision, reason, code := guardHookAnswer(t, input)
		if decision != "deny" || code != 0 || !strings.HasPrefix(reason, GuardRefusal) {
			t.Errorf("%s: %q %q exit %d", name, decision, reason, code)
		}
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

func TestGuardHookBlocksWithExitTwoWhenItCannotAnswer(t *testing.T) {
	if code := GuardHook(strings.NewReader(`{`), failingWriter{}, Guard); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}

func TestPosixQuoteKeepsAQuoteInsideOneWord(t *testing.T) {
	if got := PosixQuote(`C:/a'b/c d`); got != `'C:/a'\''b/c d'` {
		t.Fatalf("got %s", got)
	}
}
