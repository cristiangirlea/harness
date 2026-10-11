package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setup(t *testing.T) (*Workspace, map[string]Tool) {
	t.Helper()
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, "src", "deep"), 0o755))
	must(t, os.MkdirAll(filepath.Join(dir, "node_modules", "x"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "src", "a.go"), []byte("package a\n\nfunc Hello() {}\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "src", "deep", "b.go"), []byte("package deep\n// TODO fix\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "node_modules", "x", "c.go"), []byte("// TODO ignored\n"), 0o644))
	ws, err := NewWorkspace(dir)
	must(t, err)
	byName := map[string]Tool{}
	for _, tl := range All() {
		byName[tl.Name] = tl
	}
	return ws, byName
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func call(t *testing.T, ws *Workspace, tl Tool, in map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(in)
	return tl.Run(context.Background(), ws, raw)
}

func TestResolveRejectsEscapes(t *testing.T) {
	ws, _ := setup(t)
	for _, p := range []string{"..", "../x", "src/../../x", filepath.Dir(ws.Root)} {
		if _, err := ws.Resolve(p); err == nil {
			t.Errorf("Resolve(%q) should fail", p)
		}
	}
	if _, err := ws.Resolve("src/new/file.txt"); err != nil {
		t.Errorf("non-existent path inside workspace should resolve: %v", err)
	}
}

func TestReadFileLineNumbers(t *testing.T) {
	ws, tl := setup(t)
	out, err := call(t, ws, tl["read_file"], map[string]any{"path": "src/a.go", "offset": 3, "limit": 1})
	must(t, err)
	if !strings.HasPrefix(out, "3\tfunc Hello() {}") {
		t.Fatalf("unexpected output %q", out)
	}
}

func TestEditFileUniqueness(t *testing.T) {
	ws, tl := setup(t)
	p := filepath.Join(ws.Root, "dup.txt")
	must(t, os.WriteFile(p, []byte("x\nx\n"), 0o644))

	if _, err := call(t, ws, tl["edit_file"], map[string]any{"path": "dup.txt", "old_string": "x", "new_string": "y"}); err == nil {
		t.Fatal("ambiguous edit should fail")
	}
	_, err := call(t, ws, tl["edit_file"], map[string]any{"path": "dup.txt", "old_string": "x", "new_string": "y", "replace_all": true})
	must(t, err)
	data, _ := os.ReadFile(p)
	if string(data) != "y\ny\n" {
		t.Fatalf("got %q", data)
	}
}

func TestEditFileCRLF(t *testing.T) {
	ws, tl := setup(t)
	p := filepath.Join(ws.Root, "crlf.txt")
	must(t, os.WriteFile(p, []byte("one\r\ntwo\r\n"), 0o644))
	_, err := call(t, ws, tl["edit_file"], map[string]any{"path": "crlf.txt", "old_string": "one\ntwo", "new_string": "1\n2"})
	must(t, err)
	data, _ := os.ReadFile(p)
	if string(data) != "1\r\n2\r\n" {
		t.Fatalf("got %q", data)
	}
}

func TestListFilesAndGrepSkipVendorDirs(t *testing.T) {
	ws, tl := setup(t)
	out, err := call(t, ws, tl["list_files"], map[string]any{"pattern": "**/*.go"})
	must(t, err)
	if out != "src/a.go\nsrc/deep/b.go" {
		t.Fatalf("list_files got %q", out)
	}
	out, err = call(t, ws, tl["grep"], map[string]any{"pattern": "todo", "case_insensitive": true})
	must(t, err)
	if strings.TrimSpace(out) != "src/deep/b.go:2:// TODO fix" {
		t.Fatalf("grep got %q", out)
	}
}

func TestGrepDoesNotFollowASymlinkOutOfTheWorkspace(t *testing.T) {
	ws, tl := setup(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	must(t, os.WriteFile(outside, []byte("outside TODO\n"), 0o644))
	if err := os.Symlink(outside, filepath.Join(ws.Root, "src", "link.txt")); err != nil {
		t.Skipf("symbolic links need a privilege here: %v", err)
	}
	out, err := call(t, ws, tl["grep"], map[string]any{"pattern": "TODO"})
	must(t, err)
	if strings.Contains(out, "outside") {
		t.Errorf("grep followed a symbolic link out of the workspace: %q", out)
	}
	if !strings.Contains(out, "src/deep/b.go:2:// TODO fix") {
		t.Errorf("grep lost the workspace's own match: %q", out)
	}
	out, err = call(t, ws, tl["grep"], map[string]any{"pattern": "TODO", "glob": "**/*.txt"})
	must(t, err)
	if out != "(no matches)" {
		t.Errorf("grep with a glob followed a symbolic link out of the workspace: %q", out)
	}
}

func TestGlobRegexp(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{"**/*.go", "a.go", true},
		{"**/*.go", "x/y/a.go", true},
		{"*.go", "x/a.go", false},
		{"src/**", "src/a/b", true},
		{"*.{ts,tsx}", "a.tsx", true},
	}
	for _, c := range cases {
		re, err := GlobRegexp(c.glob)
		must(t, err)
		if got := re.MatchString(c.path); got != c.want {
			t.Errorf("%s ~ %s = %v, want %v", c.glob, c.path, got, c.want)
		}
	}
}

func TestRunCommandExitCode(t *testing.T) {
	ws, tl := setup(t)
	out, err := call(t, ws, tl["run_command"], map[string]any{"command": "echo hi && exit 3"})
	if err == nil || !strings.Contains(out, "hi") || !strings.Contains(out, "[exit code 3]") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}
