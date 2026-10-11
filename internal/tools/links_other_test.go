//go:build !windows

package tools

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFileOnADanglingLinkOutIsRefused(t *testing.T) {
	ws, tl := setup(t)
	outside := t.TempDir()
	must(t, os.Symlink(filepath.Join(outside, "created.txt"), filepath.Join(ws.Root, "s")))
	must(t, os.Symlink(filepath.Join(outside, "dir"), filepath.Join(ws.Root, "d")))
	// A chain: the first link stays inside, the second leads out.
	must(t, os.Symlink("t", filepath.Join(ws.Root, "c")))
	must(t, os.Symlink(filepath.Join(outside, "chained.txt"), filepath.Join(ws.Root, "t")))
	// Read as text, evil/../dotdot.txt is in the workspace; the system takes
	// the .. after following evil, which lands it outside.
	must(t, os.Mkdir(filepath.Join(outside, "deep"), 0o755))
	must(t, os.Symlink(filepath.Join(outside, "deep"), filepath.Join(ws.Root, "evil")))
	must(t, os.Symlink("evil/../dotdot.txt", filepath.Join(ws.Root, "dd"))) // not Join: it cleans

	for _, p := range []string{"s", "d/x.txt", "c", "dd"} {
		if _, err := ws.Resolve(p); err == nil {
			t.Errorf("Resolve(%q) through a dangling link out was not refused", p)
		}
		if _, err := call(t, ws, tl["write_file"], map[string]any{"path": p, "content": "x"}); err == nil {
			t.Errorf("write_file %s through a dangling link out was not refused", p)
		}
	}
	entries, err := os.ReadDir(outside)
	must(t, err)
	for _, e := range entries {
		if e.Name() != "deep" {
			t.Errorf("write_file created %s outside the workspace", e.Name())
		}
	}
}

func TestWriteFileOnADanglingLinkInsideCreatesItsTarget(t *testing.T) {
	ws, tl := setup(t)
	must(t, os.Symlink(filepath.Join("src", "new.txt"), filepath.Join(ws.Root, "rel")))
	must(t, os.Symlink(filepath.Join(ws.Root, "src", "abs.txt"), filepath.Join(ws.Root, "abs")))
	must(t, os.Symlink(filepath.Join("..", "up.txt"), filepath.Join(ws.Root, "src", "up")))

	for p, target := range map[string]string{"rel": "src/new.txt", "abs": "src/abs.txt", "src/up": "up.txt"} {
		_, err := call(t, ws, tl["write_file"], map[string]any{"path": p, "content": "inside"})
		must(t, err)
		data, err := os.ReadFile(filepath.Join(ws.Root, target))
		must(t, err)
		if string(data) != "inside" {
			t.Errorf("%s holds %q", target, data)
		}
	}
}

// The system follows each link's target on its own, so a chain can reach a
// place whose whole path is longer than any one path hrn can look at.
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

func TestADanglingLinkLoopIsRefused(t *testing.T) {
	ws, tl := setup(t)
	must(t, os.Symlink("b", filepath.Join(ws.Root, "a")))
	must(t, os.Symlink("a", filepath.Join(ws.Root, "b")))
	if _, err := ws.Resolve("a"); err == nil {
		t.Error("Resolve through a loop of links was not refused")
	}
	if _, err := call(t, ws, tl["write_file"], map[string]any{"path": "a", "content": "x"}); err == nil {
		t.Error("write_file through a loop of links was not refused")
	}
	if _, err := os.Lstat(filepath.Join(ws.Root, "a")); errors.Is(err, fs.ErrNotExist) {
		t.Error("the link itself is gone")
	}
}
