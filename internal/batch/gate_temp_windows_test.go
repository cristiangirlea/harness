package batch

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGateTempRejectsJunction(t *testing.T) {
	base := t.TempDir()
	tree := filepath.Join(base, "repo", ".hrn", "slots", "1")
	if err := os.MkdirAll(tree, 0755); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(base, ".hrn-gate-tmp")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", parent, t.TempDir()).CombinedOutput(); err != nil {
		t.Skipf("mklink /J failed: %v: %s", err, out)
	}
	got, err := filepath.EvalSymlinks(gateTempParent(tree))
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("parent = %q, want %q", got, want)
	}
}
