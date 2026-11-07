package tools

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFileAndEditFileInsideTheWorkspaceKeepTheirContents(t *testing.T) {
	ws, tl := setup(t)
	for _, content := range []string{"a longer original value", "short"} {
		_, err := call(t, ws, tl["write_file"], map[string]any{"path": "new/file.txt", "content": content})
		must(t, err)
		data, err := os.ReadFile(filepath.Join(ws.Root, "new", "file.txt"))
		must(t, err)
		if string(data) != content {
			t.Fatalf("write_file left %q, want %q", data, content)
		}
	}
	_, err := call(t, ws, tl["edit_file"], map[string]any{"path": "new/file.txt", "old_string": "short", "new_string": "ok"})
	must(t, err)
	data, err := os.ReadFile(filepath.Join(ws.Root, "new", "file.txt"))
	must(t, err)
	if string(data) != "ok" {
		t.Fatalf("edit_file left %q", data)
	}
}

func fileToolsRefuseParentSwap(t *testing.T, link func(*testing.T, string, string)) {
	t.Helper()
	for _, stage := range []string{"write_file", "read_file", "edit_file/read", "edit_file/write"} {
		tool := strings.Split(stage, "/")[0]
		for _, existing := range []bool{false, true} {
			if !existing && tool != "write_file" {
				continue
			}
			t.Run(stage+map[bool]string{false: "/new", true: "/existing"}[existing], func(t *testing.T) {
				ws, tl := setup(t)
				outside := t.TempDir()
				parent := filepath.Join(ws.Root, "swap")
				must(t, os.Mkdir(parent, 0o755))
				must(t, os.WriteFile(filepath.Join(parent, "file.txt"), []byte("original"), 0o644))
				if existing {
					must(t, os.WriteFile(filepath.Join(outside, "file.txt"), []byte("original"), 0o644))
				}
				fired := false
				opens := 0
				beforeOpenInside = func(string) {
					opens++
					if stage == "edit_file/write" && opens == 1 {
						return
					}
					beforeOpenInside = nil
					fired = true
					must(t, os.Rename(parent, parent+"-saved"))
					link(t, parent, outside)
					if !existing {
						entries, err := os.ReadDir(outside)
						must(t, err)
						if len(entries) != 0 {
							t.Fatal("outside directory was not empty before opening")
						}
					}
				}
				t.Cleanup(func() { beforeOpenInside = nil })
				_, err := call(t, ws, tl[tool], map[string]any{"path": "swap/file.txt", "content": "changed", "old_string": "original", "new_string": "changed"})
				if !fired || err == nil {
					t.Errorf("%s: swap called = %v, error = %v; want swap and refusal", tool, fired, err)
				}
				if stage == "edit_file/read" && opens != 1 {
					t.Errorf("edit_file reached %d opens; want refusal on the first read", opens)
				}
				if stage == "edit_file/read" {
					var openErr *os.PathError
					if !errors.As(err, &openErr) || openErr.Path != filepath.Join("swap", "file.txt") {
						t.Errorf("edit_file refusal = %v; want the read's root-relative open error", err)
					}
				}
				t.Logf("%s refused swapped parent: %v", tool, err)
				entries, err := os.ReadDir(outside)
				must(t, err)
				if !existing {
					if len(entries) != 0 {
						t.Fatalf("outside directory gained %d entries", len(entries))
					}
					return
				}
				data, err := os.ReadFile(filepath.Join(outside, "file.txt"))
				must(t, err)
				if len(entries) != 1 || string(data) != "original" {
					t.Fatalf("outside directory changed: %d entries, content %q", len(entries), data)
				}
			})
		}
	}
}

func writeFileRefusesParentSwapBeforeCreatingDirectories(t *testing.T, link func(*testing.T, string, string)) {
	t.Helper()
	ws, tl := setup(t)
	outside := t.TempDir()
	parent := filepath.Join(ws.Root, "a")
	must(t, os.Mkdir(parent, 0o755))
	fired := false
	beforeOpenInside = func(string) {
		beforeOpenInside = nil
		fired = true
		if _, err := os.Stat(filepath.Join(parent, "b")); !os.IsNotExist(err) {
			t.Errorf("parent directories created before safe opening: %v", err)
		}
		must(t, os.Rename(parent, parent+"-saved"))
		link(t, parent, outside)
		entries, err := os.ReadDir(outside)
		must(t, err)
		if len(entries) != 0 {
			t.Fatal("outside directory was not empty before opening")
		}
	}
	t.Cleanup(func() { beforeOpenInside = nil })
	_, err := call(t, ws, tl["write_file"], map[string]any{"path": "a/b/new/file.txt", "content": "changed"})
	if !fired || err == nil {
		t.Errorf("swap called = %v, error = %v; want swap and refusal", fired, err)
	}
	t.Logf("write_file refused swapped ancestor: %v", err)
	entries, err := os.ReadDir(outside)
	must(t, err)
	if len(entries) != 0 {
		t.Fatalf("outside directory gained %d entries", len(entries))
	}
}

func TestWriteFileThroughALinkInsideTheWorkspaceUpdatesItsTarget(t *testing.T) {
	ws, tl := setup(t)
	target := filepath.Join(ws.Root, "target.txt")
	must(t, os.WriteFile(target, []byte("original longer content"), 0o644))
	if err := os.Symlink(target, filepath.Join(ws.Root, "alias.txt")); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	_, err := call(t, ws, tl["write_file"], map[string]any{"path": "alias.txt", "content": "updated"})
	must(t, err)
	data, err := os.ReadFile(target)
	must(t, err)
	if string(data) != "updated" {
		t.Fatalf("target contains %q", data)
	}
	info, err := os.Lstat(filepath.Join(ws.Root, "alias.txt"))
	must(t, err)
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("write_file replaced the link")
	}
}

func fileToolsRefuseASecretSwappedInInsideTheWorkspace(t *testing.T, link func(*testing.T, string, string)) {
	t.Helper()
	// "new" writes a name that does not exist yet; "final" swaps the file itself rather than its parent.
	for _, stage := range []string{"write_file", "write_file/new", "write_file/final", "read_file", "read_file/final", "edit_file/read", "edit_file/write"} {
		tool := strings.Split(stage, "/")[0]
		t.Run(stage, func(t *testing.T) {
			ws, tl := setup(t)
			parent := filepath.Join(ws.Root, "swap")
			file := filepath.Join(parent, "file.txt")
			secret := filepath.Join(ws.Root, ".ssh", "file.txt")
			must(t, os.Mkdir(parent, 0o755))
			must(t, os.MkdirAll(filepath.Dir(secret), 0o755))
			if stage != "write_file/new" {
				must(t, os.WriteFile(file, []byte("original"), 0o644))
			}
			must(t, os.WriteFile(secret, []byte("original SECRET"), 0o644))
			opens := 0
			beforeOpenInside = func(string) {
				opens++
				if stage == "edit_file/write" && opens == 1 {
					return
				}
				beforeOpenInside = nil
				if strings.HasSuffix(stage, "/final") {
					must(t, os.Remove(file))
					relativeSymlink(t, file, secret)
					return
				}
				must(t, os.Rename(parent, parent+"-saved"))
				link(t, parent, filepath.Dir(secret))
			}
			t.Cleanup(func() { beforeOpenInside = nil })
			out, err := call(t, ws, tl[tool], map[string]any{"path": "swap/file.txt", "content": "changed", "old_string": "original", "new_string": "changed"})
			if err == nil {
				t.Errorf("%s through a swapped-in secret: output %q, want a refusal", stage, out)
			}
			t.Logf("%s refused the swapped-in secret: %v", stage, err)
			if strings.Contains(out, "SECRET") || (err != nil && strings.Contains(err.Error(), "SECRET")) {
				t.Errorf("%s showed the secret file: %q, %v", stage, out, err)
			}
			data, err := os.ReadFile(secret)
			must(t, err)
			if string(data) != "original SECRET" {
				t.Fatalf("%s changed the secret file to %q", stage, data)
			}
		})
	}
}

func openInsideRefusesASecretReachedThroughALink(t *testing.T, link func(*testing.T, string, string)) {
	t.Helper()
	ws, _ := setup(t)
	must(t, os.MkdirAll(filepath.Join(ws.Root, ".ssh"), 0o755))
	must(t, os.WriteFile(filepath.Join(ws.Root, ".ssh", "file.txt"), []byte("SECRET"), 0o644))
	link(t, filepath.Join(ws.Root, "swap"), filepath.Join(ws.Root, ".ssh"))
	p, err := ws.Resolve("swap/file.txt")
	must(t, err)
	f, _, err := ws.openInside(p, os.O_RDONLY, 0)
	if err == nil {
		f.Close()
		t.Fatal("openInside opened a secret file through a link")
	}
	if !strings.Contains(err.Error(), "secret-file") {
		t.Fatalf("refusal = %v, want the secret-file guard", err)
	}
}

// A relative link stays inside the root, so os.Root follows it when opening.
func relativeSymlink(t *testing.T, link, target string) {
	t.Helper()
	rel, err := filepath.Rel(filepath.Dir(link), target)
	must(t, err)
	if err := os.Symlink(rel, link); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
}
