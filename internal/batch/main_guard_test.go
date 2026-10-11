package batch

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/tools"
)

// The tests play the operator, who may lift the guard, even when an agent
// under the guard runs them (HRN_GUARDED is set there). A helper process,
// the test binary standing in for a CLI or an MCP server, keeps what it was
// started with: that is what it reports.
func TestMain(m *testing.M) {
	helper := slices.ContainsFunc(os.Args, func(a string) bool { return strings.HasPrefix(a, "-test.run=^TestHelper") })
	if !helper {
		os.Unsetenv(tools.GuardedVar)
	}
	os.Exit(m.Run())
}
