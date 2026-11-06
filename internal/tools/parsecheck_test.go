package tools

import (
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
		{"text is unchecked", "a.txt", "", "", "{", "{", true, false, false},
		{"markdown is unchecked", "a.md", "", "", "{", "{", true, false, false},
		{"valid CRLF Go edit is written", "a.go", "package a\r\nvar x = 1\r\n", "var x = 1\n", "var x = 2\n", "package a\r\nvar x = 2\r\n", true, true, false},
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
				if tc.path == "a.JSON" && !strings.Contains(err.Error(), ": 2:8: invalid character '}'") {
					t.Fatalf("wrong JSON byte position: %v", err)
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
