package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestHelperGuardHook is not a test: the test binary runs as "hrn guard-hook".
func TestHelperGuardHook(t *testing.T) {
	if os.Getenv("HRN_GUARD_HOOK_HELPER") == "" {
		t.Skip("helper")
	}
	os.Args = []string{"hrn", "guard-hook"}
	main()
}

func runGuardHook(t *testing.T, command string) (string, int) {
	t.Helper()
	input, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": map[string]string{"command": command}})
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperGuardHook$")
	cmd.Env = append(os.Environ(), "HRN_GUARD_HOOK_HELPER=1")
	cmd.Stdin = bytes.NewReader(input)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return stdout.String(), code
}

func TestGuardHookCommandRefusesAGuardedCommandAndADenyRule(t *testing.T) {
	for _, command := range []string{"git push origin x", "gh repo edit --description x"} {
		out, code := runGuardHook(t, command)
		if code != 0 || !strings.Contains(out, `"permissionDecision":"deny"`) {
			t.Errorf("%q: exit %d, answer %q", command, code, out)
		}
	}
}

func TestGuardHookCommandSaysNothingOfAnAllowedCommand(t *testing.T) {
	if out, code := runGuardHook(t, "go test ./..."); code != 0 || out != "" {
		t.Fatalf("exit %d, answer %q", code, out)
	}
}
