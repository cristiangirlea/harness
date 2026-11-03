package platform

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

var integrationHRN struct {
	sync.Once
	binary []byte
	err    error
}

func realHRN(t *testing.T) string {
	t.Helper()
	t.Setenv("HRN_HOME", t.TempDir())
	dir := t.TempDir()
	name := "hrn"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(dir, name)
	integrationHRN.Do(func() {
		cmd := exec.Command("go", "build", "-o", path, "./cmd/hrn")
		cmd.Dir = filepath.Join("..", "..")
		if output, err := cmd.CombinedOutput(); err != nil {
			integrationHRN.err = fmt.Errorf("build hrn: %w: %s", err, output)
			return
		}
		integrationHRN.binary, integrationHRN.err = os.ReadFile(path)
	})
	if integrationHRN.err != nil {
		t.Fatal(integrationHRN.err)
	}
	if err := os.WriteFile(path, integrationHRN.binary, 0755); err != nil {
		t.Fatal(err)
	}
	return path
}
