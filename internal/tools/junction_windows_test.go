//go:build windows

package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// junction makes a directory junction, which needs no privilege (a symbolic
// link does, unless Developer Mode is on).
func junction(t *testing.T, link, target string) {
	t.Helper()
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J %s %s: %v: %s", link, target, err, out)
	}
}

// outsideWithJunction makes a directory outside the workspace holding
// secret.txt, and a junction "out" in the workspace that leads to it.
func outsideWithJunction(t *testing.T, ws *Workspace) string {
	t.Helper()
	outside := t.TempDir()
	must(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside TODO\n"), 0o644))
	must(t, os.MkdirAll(filepath.Join(outside, "sub"), 0o755))
	must(t, os.WriteFile(filepath.Join(outside, "sub", "deeper.txt"), []byte("outside TODO\n"), 0o644))
	junction(t, filepath.Join(ws.Root, "out"), outside)
	return outside
}

func TestFileToolsDoNotFollowAJunctionOutOfTheWorkspace(t *testing.T) {
	ws, tl := setup(t)
	outside := outsideWithJunction(t, ws)

	for _, p := range []string{"out", "out/secret.txt", "out/sub/deeper.txt", "out/new.txt", "out/sub/new/file.txt"} {
		if _, err := ws.Resolve(p); err == nil {
			t.Errorf("Resolve(%q) went through the junction out of the workspace", p)
		}
	}
	if out, err := call(t, ws, tl["read_file"], map[string]any{"path": "out/secret.txt"}); err == nil {
		t.Errorf("read_file read outside the workspace: %q", out)
	}
	if _, err := call(t, ws, tl["write_file"], map[string]any{"path": "out/new.txt", "content": "x"}); err == nil {
		t.Error("write_file wrote through the junction")
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); err == nil {
		t.Error("write_file created a file outside the workspace")
	}
	if _, err := call(t, ws, tl["edit_file"], map[string]any{"path": "out/secret.txt", "old_string": "outside", "new_string": "changed"}); err == nil {
		t.Error("edit_file edited through the junction")
	}
	if data, _ := os.ReadFile(filepath.Join(outside, "secret.txt")); string(data) != "outside TODO\n" {
		t.Errorf("the file outside the workspace changed: %q", data)
	}
}

func TestGrepDoesNotSearchThroughAJunctionOutOfTheWorkspace(t *testing.T) {
	ws, tl := setup(t)
	outsideWithJunction(t, ws)

	for _, p := range []string{"out", "out/sub", "out/secret.txt"} {
		if out, err := call(t, ws, tl["grep"], map[string]any{"pattern": "TODO", "path": p}); err == nil {
			t.Errorf("grep in %q searched outside the workspace: %q", p, out)
		}
	}
	out, err := call(t, ws, tl["grep"], map[string]any{"pattern": "TODO"})
	must(t, err)
	if strings.Contains(out, "outside") {
		t.Errorf("grep over the workspace searched through the junction: %q", out)
	}
}

func TestAJunctionThatStaysInTheWorkspaceIsFollowed(t *testing.T) {
	ws, tl := setup(t)
	// The shape pnpm gives node_modules on Windows: junctions to elsewhere in
	// the project.
	junction(t, filepath.Join(ws.Root, "alias"), filepath.Join(ws.Root, "src"))

	out, err := call(t, ws, tl["read_file"], map[string]any{"path": "alias/a.go"})
	must(t, err)
	if !strings.Contains(out, "func Hello") {
		t.Fatalf("read_file through the junction got %q", out)
	}
	_, err = call(t, ws, tl["write_file"], map[string]any{"path": "alias/new.go", "content": "package a\n"})
	must(t, err)
	if _, err := os.Stat(filepath.Join(ws.Root, "src", "new.go")); err != nil {
		t.Fatalf("write_file through the junction: %v", err)
	}
	// grep's walk judges each file where it really is, so a file reached
	// through the junction is searched. (A walk does not descend into a
	// junction or link it is given as its own start, as before.)
	out, err = call(t, ws, tl["grep"], map[string]any{"pattern": "TODO", "path": "alias/deep"})
	must(t, err)
	if !strings.Contains(out, "TODO fix") {
		t.Fatalf("grep through the junction got %q", out)
	}
}

func TestAWorkspaceReachedThroughAJunctionStillWorks(t *testing.T) {
	real, _ := setup(t)
	link := filepath.Join(t.TempDir(), "link")
	junction(t, link, real.Root)
	ws, err := NewWorkspace(link)
	must(t, err)
	byName := map[string]Tool{}
	for _, tl := range All() {
		byName[tl.Name] = tl
	}

	out, err := call(t, ws, byName["read_file"], map[string]any{"path": "src/a.go"})
	must(t, err)
	if !strings.Contains(out, "func Hello") {
		t.Fatalf("read_file got %q", out)
	}
	if _, err := call(t, ws, byName["write_file"], map[string]any{"path": "src/new/file.txt", "content": "x"}); err != nil {
		t.Fatalf("write_file in a workspace reached through a junction: %v", err)
	}
	for _, p := range []string{"..", "../x", filepath.Dir(real.Root)} {
		if _, err := ws.Resolve(p); err == nil {
			t.Errorf("Resolve(%q) should fail", p)
		}
	}
}
