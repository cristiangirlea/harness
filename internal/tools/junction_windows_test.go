//go:build windows

package tools

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

func TestJunctionsThatLoopLeadNowhereOrTakeTwoHopsAreJudged(t *testing.T) {
	ws, tl := setup(t)
	outside := outsideWithJunction(t, ws)
	// Two junctions that lead to each other: the path is refused, and the
	// call returns.
	a, b := filepath.Join(ws.Root, "a"), filepath.Join(ws.Root, "b")
	junction(t, a, b) // a junction's target need not exist
	junction(t, b, a)
	if _, err := ws.Resolve("a/x.txt"); err == nil {
		t.Error("Resolve went round a loop of junctions and let the path through")
	}
	// A junction to a folder outside that does not exist yet.
	junction(t, filepath.Join(ws.Root, "later"), filepath.Join(outside, "later"))
	if _, err := call(t, ws, tl["write_file"], map[string]any{"path": "later/new.txt", "content": "x"}); err == nil {
		t.Error("write_file went through a junction to a folder not there yet")
	}
	if _, err := os.Stat(filepath.Join(outside, "later")); err == nil {
		t.Error("write_file created a folder outside the workspace")
	}
	// A junction that stays inside, to a folder holding the junction out.
	must(t, os.MkdirAll(filepath.Join(ws.Root, "hop"), 0o755))
	junction(t, filepath.Join(ws.Root, "hop", "out"), outside)
	junction(t, filepath.Join(ws.Root, "first"), filepath.Join(ws.Root, "hop"))
	if out, err := call(t, ws, tl["read_file"], map[string]any{"path": "first/out/secret.txt"}); err == nil {
		t.Errorf("read_file went out in two hops: %q", out)
	}
}

func TestAPathWhoseLinksCannotBeJudgedIsRefusedForThatReason(t *testing.T) {
	ws, _ := setup(t)
	junction(t, filepath.Join(ws.Root, "a"), filepath.Join(ws.Root, "b"))
	junction(t, filepath.Join(ws.Root, "b"), filepath.Join(ws.Root, "a"))
	for _, p := range []string{"a/x.txt", "bad*name/x.txt"} {
		_, err := ws.Resolve(p)
		if err == nil || !strings.Contains(err.Error(), "cannot tell where it leads") {
			t.Errorf("Resolve(%q) = %v, want a refusal that says where it leads cannot be told", p, err)
		}
	}
}

func TestASymlinkThroughAJunctionOutIsRefused(t *testing.T) {
	ws, tl := setup(t)
	outside := outsideWithJunction(t, ws)
	// filepath.EvalSymlinks follows s, fails at the junction past it, and a
	// shorter prefix would leave s unresolved. links_windows_test.go shows
	// the same without the privilege this needs.
	if err := os.Symlink(filepath.Join(ws.Root, "out", "sub"), filepath.Join(ws.Root, "s")); err != nil {
		t.Skipf("symbolic links need a privilege here: %v", err)
	}
	must(t, os.Symlink(filepath.Join(outside, "new.txt"), filepath.Join(ws.Root, "dangling")))

	if out, err := call(t, ws, tl["read_file"], map[string]any{"path": "s/deeper.txt"}); err == nil {
		t.Errorf("read_file read outside the workspace: %q", out)
	}
	if _, err := call(t, ws, tl["write_file"], map[string]any{"path": "dangling", "content": "x"}); err == nil {
		t.Error("write_file wrote through a symbolic link to a file not there yet")
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); err == nil {
		t.Error("write_file created a file outside the workspace")
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
	// A path through the junction is searched. (A walk goes into no link or
	// junction, as before, so grep over the root or over "alias" itself does
	// not reach the files behind it.)
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
	// Root keeps the junction: commands run where the user said. Its folder
	// may be spelt with long names where the temp dir has short ones.
	parent, err := filepath.EvalSymlinks(filepath.Dir(link))
	must(t, err)
	if filepath.Base(ws.Root) != "link" || !strings.EqualFold(filepath.Dir(ws.Root), parent) {
		t.Errorf("Root = %s, want the junction's own path %s", ws.Root, link)
	}
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

func TestListAndGrepInJunctionRootedWorkspace(t *testing.T) {
	real, _ := setup(t)
	must(t, os.MkdirAll(filepath.Join(real.Root, "sub"), 0o755))
	must(t, os.WriteFile(filepath.Join(real.Root, "root.txt"), []byte("junction root marker\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(real.Root, "sub", "target.txt"), []byte("junction root marker\n"), 0o644))
	link := filepath.Join(t.TempDir(), "link")
	junction(t, link, real.Root)
	ws, err := NewWorkspace(link)
	must(t, err)
	outsideWithJunction(t, ws)
	byName := map[string]Tool{}
	for _, tl := range All() {
		byName[tl.Name] = tl
	}

	out, err := call(t, ws, byName["list_files"], map[string]any{})
	must(t, err)
	// Whole lines, not substrings: a path reported relative to the junction's
	// target would still contain the name ("../../x/root.txt").
	for _, path := range []string{"root.txt", "sub/target.txt"} {
		if !slices.Contains(strings.Split(out, "\n"), path) {
			t.Errorf("list_files = %q, want %q", out, path)
		}
	}
	if strings.Contains(out, "out/secret.txt") {
		t.Errorf("list_files followed the junction outside: %q", out)
	}

	for _, tc := range []struct {
		path string
		want string
	}{
		{"", "root.txt:1:junction root marker"},
		{"sub", "sub/target.txt:1:junction root marker"},
	} {
		out, err := call(t, ws, byName["grep"], map[string]any{"pattern": "junction root marker", "path": tc.path})
		must(t, err)
		if !slices.Contains(strings.Split(out, "\n"), tc.want) {
			t.Errorf("grep in %q = %q, want the line %q", tc.path, out, tc.want)
		}
	}
	out, err = call(t, ws, byName["grep"], map[string]any{"pattern": "outside TODO"})
	must(t, err)
	if out != "(no matches)" {
		t.Errorf("grep followed the junction outside: %q", out)
	}
}

// A secret directory reached through a junction inside the workspace is
// secret where the junction leads, for the file tools and for grep.
func TestASecretDirectoryBehindAJunctionIsSecret(t *testing.T) {
	ws, tl := setup(t)
	must(t, os.MkdirAll(filepath.Join(ws.Root, ".private"), 0o755))
	must(t, os.WriteFile(filepath.Join(ws.Root, ".private", "key.txt"), []byte("hunter2-private\n"), 0o644))
	junction(t, filepath.Join(ws.Root, "notes"), filepath.Join(ws.Root, ".private"))

	raw, _ := json.Marshal(map[string]any{"path": "notes/key.txt"})
	if rule := tl["read_file"].Refuse(ws, raw); rule != "secret-file" {
		t.Errorf("read_file notes/key.txt, notes -> .private: Refuse = %q, want secret-file", rule)
	}
	// Over the root the secret directory is passed over; on the junction
	// itself the search is refused. Either way nothing in it is shown.
	for _, path := range []string{"", "notes"} {
		if out, err := call(t, ws, tl["grep"], map[string]any{"pattern": "hunter2", "path": path}); strings.Contains(out, "hunter2-private") {
			t.Errorf("grep in %q = %q, %v; want nothing from the secret directory", path, out, err)
		}
	}
}

// A workspace reached through a junction is judged from where it really is:
// from the junction's side, the folders above the real workspace would read as
// part of every path, and a parent named .private would make all of it secret.
func TestAWorkspaceReachedThroughAJunctionIsJudgedFromItsRealRoot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".private", "repo")
	must(t, os.MkdirAll(dir, 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main // hunter2-ok\n"), 0o644))
	link := filepath.Join(t.TempDir(), "link")
	junction(t, link, dir)
	ws, err := NewWorkspace(link)
	must(t, err)
	byName := map[string]Tool{}
	for _, tl := range All() {
		byName[tl.Name] = tl
	}

	raw, _ := json.Marshal(map[string]any{"path": "main.go"})
	if rule := byName["read_file"].Refuse(ws, raw); rule != "" {
		t.Errorf("main.go in a workspace reached through a junction into .private: Refuse = %q, want none", rule)
	}
	if out, err := call(t, ws, byName["read_file"], map[string]any{"path": "main.go"}); err != nil || !strings.Contains(out, "hunter2-ok") {
		t.Errorf("read_file = %q, %v; want main.go read", out, err)
	}
}
