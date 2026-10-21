package batch

import (
	"os"
	"path/filepath"
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
		// A temporary HRN_HOME, so no test reads the user's records or
		// backends.json (#379); a test may still set its own with t.Setenv.
		home, err := os.MkdirTemp("", "hrn-test-home-")
		if err != nil {
			panic(err)
		}
		os.Setenv("HRN_HOME", home)
		code := m.Run()
		os.RemoveAll(home)
		if repoTemplate != "" {
			os.RemoveAll(repoTemplate)
		}
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// The tests never read or write the real ~/.hrn: backends.json there could
// disable the backend a test names (#379).
func TestTheTestsRunAgainstATemporaryHRNHome(t *testing.T) {
	home := os.Getenv("HRN_HOME")
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(home)
	if home == "" || err != nil || !strings.HasPrefix(got, tmp) {
		t.Fatalf("HRN_HOME = %q, want a directory under %s", home, tmp)
	}
}
