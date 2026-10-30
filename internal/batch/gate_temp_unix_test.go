//go:build unix

package batch

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGateTempRejectsUnsafeParent(t *testing.T) {
	for _, kind := range []string{"symlink", "world-writable"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			tree := filepath.Join(base, "repo", ".hrn", "slots", "1")
			if err := os.MkdirAll(tree, 0755); err != nil {
				t.Fatal(err)
			}
			parent := filepath.Join(base, ".hrn-gate-tmp")
			if kind == "symlink" {
				if err := os.Symlink(t.TempDir(), parent); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(parent, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(parent, 0777); err != nil {
					t.Fatal(err)
				}
			}
			if got := gateTempParent(tree); got != os.TempDir() {
				t.Errorf("parent = %q, want %q", got, os.TempDir())
			}
		})
	}
}
