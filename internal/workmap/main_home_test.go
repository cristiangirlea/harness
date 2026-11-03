package workmap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "hrn-workmap-test-")
	if err != nil {
		panic(err)
	}
	os.Setenv("CLAUDE_CONFIG_DIR", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

func TestClaudeConfigUsesATemporaryDirectory(t *testing.T) {
	home := os.Getenv("CLAUDE_CONFIG_DIR")
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(home)
	rel, relErr := filepath.Rel(tmp, got)
	if home == "" || err != nil || relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q", home)
	}
}
