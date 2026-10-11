package hrnmcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/tools"
)

// refused calls a tool that must refuse, and returns what it said.
func refused(t *testing.T, s *session, tool, args string) string {
	t.Helper()
	res, err := s.cl.CallTool(context.Background(), tool, json.RawMessage(args))
	if err != nil {
		return err.Error()
	}
	if !res.IsError {
		t.Errorf("%s %s = %q; want it refused", tool, args, res.Text())
	}
	return res.Text()
}

// The server's caller is an agent, perhaps one under the guard whose CLI
// loaded it, and some CLIs start their MCP servers without passing their
// environment on. So the server itself refuses allow_dangerous, and does not
// offer it, unless the operator started it with --allow-dangerous-runs.
func TestTheServerRefusesToLiftTheGuardUnlessTheOperatorSaidSo(t *testing.T) {
	s := connect(t, t.TempDir())
	for _, tool := range s.cl.Tools() {
		var schema struct {
			Properties map[string]any `json:"properties"`
		}
		_ = json.Unmarshal(tool.InputSchema, &schema)
		if tool.Name == "run" && (schema.Properties["prompt"] == nil || schema.Properties["allow_dangerous"] != nil) {
			t.Errorf("run's schema offers %v; want allow_dangerous left out where it is refused", schema.Properties)
		}
	}
	for tool, args := range map[string]string{
		"run":   `{"prompt":"push it","backend":"stub","allow_dangerous":true}`,
		"batch": `{"spec":{"tasks":[{"id":"a","prompt":"push it","allow_dangerous":true}]}}`,
	} {
		if text := refused(t, s, tool, args); !strings.Contains(text, "only the operator") || strings.Contains(text, "--allow-dangerous-runs") {
			t.Errorf("%s: %q; want the refusal, naming no way around it", tool, text)
		}
	}
	if text := refused(t, s, "batch", `{"spec":{"allow_dangerous":true,"tasks":[{"id":"a","prompt":"p"}]}}`); !strings.Contains(text, "only the operator") {
		t.Errorf("batch with allow_dangerous on the spec: %q", text)
	}
}

// The second layer: a server the operator let lift the guard, but started
// inside a guarded run (its CLI passed the environment on), still refuses.
func TestRunToolRefusesToLiftTheGuardInsideAGuardedRun(t *testing.T) {
	s := connectDangerous(t, t.TempDir())
	t.Setenv(tools.GuardedVar, "1")
	if text := refused(t, s, "run", `{"prompt":"push it","backend":"stub","allow_dangerous":true}`); !strings.Contains(text, "guarded run") {
		t.Errorf("run with allow_dangerous inside a guarded run = %q; want the guarded-run refusal", text)
	}
}
