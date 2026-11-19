package prefstest

import (
	"os"
	"path/filepath"
	"testing"
	_ "unsafe"
)

// Dependency tests need the private override because Go omits a dependency's export_test.go.
//
//go:linkname orgPathOverride github.com/cristiangirlea/harness/internal/prefs.orgPathOverride
var orgPathOverride string

func TempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func OrgPath(t *testing.T) string {
	t.Helper()
	previous := orgPathOverride
	orgPathOverride = filepath.Join(TempDir(t), "backends.json")
	t.Cleanup(func() { orgPathOverride = previous })
	return orgPathOverride
}

func Write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}
