package followup

import (
	"os"
	"path/filepath"
	"testing"
)

// Routing reads codex's sessions and claude's runs for their quota; the user's must not move a test's run.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "hrn-test-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("HRN_HOME", home)
	os.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
