//go:build linux

package tools

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The system follows each link's target on its own, so a chain can reach a
// place whose whole path is longer than any one path hrn can look at. Linux
// only: the directories are longer than macOS' whole PATH_MAX (1,024).
func TestADanglingLinkPastThePathLengthLimitIsRefused(t *testing.T) {
	ws, tl := setup(t)
	outside := t.TempDir()
	long := strings.Repeat(strings.Repeat("x", 200)+"/", 12) // 2,412 bytes
	a, b := "A"+long, "B"+long
	must(t, os.MkdirAll(filepath.Join(ws.Root, a), 0o755))
	t.Chdir(filepath.Join(ws.Root, a))
	must(t, os.MkdirAll(b, 0o755))
	must(t, os.Symlink(filepath.Join(outside, "new.txt"), b+"out"))
	must(t, os.Symlink(b+"out", "pivot"))
	must(t, os.Symlink(a+"pivot", filepath.Join(ws.Root, "s")))

	if _, err := ws.Resolve("s"); err == nil {
		t.Error("Resolve through a chain past the path length limit was not refused")
	}
	if _, err := call(t, ws, tl["write_file"], map[string]any{"path": "s", "content": "x"}); err == nil {
		t.Error("write_file through a chain past the path length limit was not refused")
	}
	if _, err := os.Lstat(filepath.Join(outside, "new.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("write_file created new.txt outside the workspace (%v)", err)
	}
}
