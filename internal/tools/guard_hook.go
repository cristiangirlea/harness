package tools

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// GuardRefusal starts every reason the guard gives, so a hook's answer can be told for the guard's.
const GuardRefusal = "blocked by the hrn guard"

const guardHookMaxInput = 1 << 20

// PosixQuote is s as one word for a POSIX shell.
func PosixQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// GuardHook is the PreToolUse hook "hrn guard-hook": the guard on a command's original text, which
// the gate's rewrite hides from claude's deny rules. What it cannot read it refuses, since a hook
// that says nothing lets the command run. rule names what refuses a command, or "".
func GuardHook(stdin io.Reader, stdout io.Writer, rule func(command string) string) int {
	deny := func(reason string) int {
		line, err := json.Marshal(map[string]map[string]string{"hookSpecificOutput": {
			"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": reason,
		}})
		if err == nil {
			_, err = stdout.Write(append(line, '\n'))
		}
		if err != nil {
			return 2
		}
		return 0
	}
	unreadable := func(why string) int { return deny(GuardRefusal + " (hook input): " + why) }
	data, err := io.ReadAll(io.LimitReader(stdin, guardHookMaxInput+1))
	if err != nil {
		return unreadable(err.Error())
	}
	if len(data) > guardHookMaxInput {
		return unreadable(fmt.Sprintf("larger than %d bytes", guardHookMaxInput))
	}
	var in struct {
		Tool  string `json:"tool_name"`
		Input struct {
			Command *string `json:"command"`
		} `json:"tool_input"`
	}
	if err := json.Unmarshal(data, &in); err != nil {
		return unreadable(err.Error())
	}
	if in.Tool != "Bash" && in.Tool != "PowerShell" {
		return 0
	}
	if in.Input.Command == nil {
		return unreadable("no command")
	}
	if name := rule(*in.Input.Command); name != "" {
		return deny(GuardError(name).Error())
	}
	return 0
}
