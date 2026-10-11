package hrnmcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/tools"
)

// The route a guarded agent would take: the claude CLI hrn starts loads the
// operator's hrn MCP server, which inherits the guarded environment. Asked to
// lift the guard from there, it refuses.
func TestRunToolRefusesToLiftTheGuardInsideAGuardedRun(t *testing.T) {
	s := connect(t, t.TempDir())
	t.Setenv(tools.GuardedVar, "1")
	res, err := s.cl.CallTool(context.Background(), "run", json.RawMessage(`{"prompt":"push it","backend":"stub","allow_dangerous":true}`))
	text := res.Text()
	if err != nil {
		text = err.Error()
	}
	if (err == nil && !res.IsError) || !strings.Contains(text, "guarded run") {
		t.Errorf("run with allow_dangerous inside a guarded run = %q (error %v); want it refused", text, err)
	}
}
