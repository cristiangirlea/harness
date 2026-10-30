//go:build unix

package batch

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

type gateTempFileInfo struct{ uid uint32 }

func (gateTempFileInfo) Name() string       { return "temp" }
func (gateTempFileInfo) Size() int64        { return 0 }
func (gateTempFileInfo) Mode() os.FileMode  { return os.ModeDir | 0700 }
func (gateTempFileInfo) ModTime() time.Time { return time.Time{} }
func (gateTempFileInfo) IsDir() bool        { return true }
func (f gateTempFileInfo) Sys() any         { return &syscall.Stat_t{Uid: f.uid} }

func TestGateTempPermissionsOwner(t *testing.T) {
	uid := uint32(os.Geteuid())
	if gateTempPermissionsSafe(gateTempFileInfo{uid: uid + 1}) {
		t.Error("accepted a directory owned by another user")
	}
	if !gateTempPermissionsSafe(gateTempFileInfo{uid: uid}) {
		t.Error("rejected a directory owned by the current user")
	}
}

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
