package tools

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestParseCheckRefusesBrokenChangesAndAllowsRepairs(t *testing.T) {
	for _, tc := range []struct {
		name, path, before, old, content, want string
		enabled, exists, refused               bool
	}{
		{"Go edit is refused unchanged", "a.go", "package a\nvar x = 1\n", "1", "(", "", true, true, true},
		{"valid Go edit is written", "a.go", "package a\nvar x = 1\n", "1", "2", "package a\nvar x = 2\n", true, true, false},
		{"invalid JSON overwrite is refused", "a.JSON", "{}", "", "{\n  \"x\": }", "", true, true, true},
		{"broken Go edit is written", "a.go", "package a\nvar x = (\n", "(", "1", "package a\nvar x = 1\n", true, true, false},
		{"broken Go may remain broken", "a.go", "package a\nvar x = (\n", "(", "[", "package a\nvar x = [\n", true, true, false},
		{"new broken Go is refused", "a.GO", "", "", "package a\nvar x = (", "", true, false, true},
		{"new invalid JSON is refused", "a.json", "", "", "{", "", true, false, true},
		{"empty new Go is refused at the start", "a.go", "", "", "", "", true, false, true},
		{"empty new JSON is refused at the start", "a.json", "", "", "", "", true, false, true},
		{"text is unchecked", "a.txt", "", "", "{", "{", true, false, false},
		{"markdown is unchecked", "a.md", "", "", "{", "{", true, false, false},
		{"valid CRLF Go edit is written", "a.go", "package a\r\nvar x = 1\r\n", "var x = 1\n", "var x = 2\n", "package a\r\nvar x = 2\r\n", true, true, false},
		{"broken CRLF Go edit is refused unchanged", "a.go", "package a\r\nvar x = 1\r\n", "var x = 1\n", "var x = )\n", "", true, true, true},
		{"disabled check writes broken Go", "a.go", "package a\nvar x = 1\n", "1", "(", "package a\nvar x = (\n", false, true, false},
		{"broken JSON overwrite is written", "a.json", "{", "", "[", "[", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, tools := setup(t)
			ws.ParseCheck = tc.enabled
			path := filepath.Join(ws.Root, tc.path)
			if tc.exists {
				must(t, os.WriteFile(path, []byte(tc.before), 0o644))
			}
			name := "write_file"
			args := map[string]any{"path": tc.path, "content": tc.content}
			if tc.old != "" {
				name = "edit_file"
				args = map[string]any{"path": tc.path, "old_string": tc.old, "new_string": tc.content}
			}
			_, err := call(t, ws, tools[name], args)
			if tc.refused {
				if err == nil || !strings.HasPrefix(err.Error(), tc.path+" would not parse, so it was not changed: ") || !regexp.MustCompile(`: [1-9][0-9]*:[1-9][0-9]*: .+`).MatchString(err.Error()) {
					t.Fatalf("expected refusal with position: %v", err)
				}
				var pathErr *os.PathError
				if errors.As(err, &pathErr) || strings.Contains(err.Error(), "blocked by the hrn guard") {
					t.Fatalf("parse refusal is not an ordinary error: %v", err)
				}
				if tc.path == "a.JSON" && !strings.Contains(err.Error(), ": 2:8: invalid character '}'") {
					t.Fatalf("wrong JSON byte position: %v", err)
				}
				if tc.content == "" && !strings.Contains(err.Error(), ": 1:1: ") {
					t.Fatalf("empty file refusal is not at the start: %v", err)
				}
			} else {
				must(t, err)
			}
			data, err := os.ReadFile(path)
			if tc.refused && !tc.exists {
				if !os.IsNotExist(err) {
					t.Fatalf("refused new file exists: %v", err)
				}
				return
			}
			must(t, err)
			want := tc.want
			if tc.refused {
				want = tc.before
			}
			if string(data) != want {
				t.Fatalf("got %q, want %q", data, want)
			}
		})
	}
}

func TestParseCheckRefusesBrokenReplaceAllWithoutChangingTheFile(t *testing.T) {
	ws, tools := setup(t)
	ws.ParseCheck = true
	const original = "package a\nvar x = 1\nvar y = 1\n"
	path := filepath.Join(ws.Root, "a.go")
	must(t, os.WriteFile(path, []byte(original), 0o644))
	_, err := call(t, ws, tools["edit_file"], map[string]any{
		"path": "a.go", "old_string": "1", "new_string": ")", "replace_all": true,
	})
	if err == nil || !strings.HasPrefix(err.Error(), "a.go would not parse, so it was not changed: 2:9: ") {
		t.Fatalf("expected parse refusal: %v", err)
	}
	data, readErr := os.ReadFile(path)
	must(t, readErr)
	if string(data) != original {
		t.Fatalf("refused replace_all changed the file: %q", data)
	}
}

func TestParseCheckRefusesBrokenContentWhenTheOldTargetCannotBeRead(t *testing.T) {
	ws, tools := setup(t)
	ws.ParseCheck = true
	path := filepath.Join(ws.Root, "broken.go")
	must(t, os.Mkdir(path, 0o755))
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("directories can be read as files here")
	}
	_, err := call(t, ws, tools["write_file"], map[string]any{
		"path": "broken.go", "content": "package a\nvar x = )\n",
	})
	if err == nil || !strings.HasPrefix(err.Error(), "broken.go would not parse, so it was not changed: 2:9: ") {
		t.Fatalf("expected positioned parse refusal instead of a read error: %v", err)
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		t.Fatalf("parse refusal exposes a filesystem error: %v", err)
	}
	info, statErr := os.Stat(path)
	must(t, statErr)
	if !info.IsDir() {
		t.Fatal("refused write changed the directory target")
	}
}

func TestParseCheckRefusalLeavesNoNewDirectory(t *testing.T) {
	ws, tools := setup(t)
	ws.ParseCheck = true
	if _, err := call(t, ws, tools["write_file"], map[string]any{"path": "pkg/a.go", "content": "package a\nvar x = ("}); err == nil {
		t.Fatal("expected refusal")
	}
	if _, err := os.Stat(filepath.Join(ws.Root, "pkg")); !os.IsNotExist(err) {
		t.Fatalf("refused write created its directory: %v", err)
	}
}
