// name: privacy-check-tests
// summary: Check the personal folder, local path and narration rules on fixture files.
// effects: read, write-workspace, exec
// parameters: none
// example: go test -count=1 ./scripts/privacy-check
// hash: f5c1e357a63462b1c1ad67bab6ef5e048afcbec3fbeebd26b3a187a73e8f6e0c
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures are put together at run time, so this file never holds what it looks for.
var (
	users   = "Users"
	work    = "__" + "Work" + "dir"
	phrase  = "the " + "owner"
	someone = "alice"
)

func writeFixtures(t *testing.T, files map[string]string) (string, []string) {
	t.Helper()
	root := t.TempDir()
	var names []string
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		names = append(names, rel)
	}
	return root, names
}

func rulesOf(r report) map[string]string {
	got := map[string]string{}
	for _, f := range r.Findings {
		got[f.File] += f.Rule + ";"
	}
	return got
}

func TestPersonalFoldersLocalPathsAndNarrationAreFound(t *testing.T) {
	root, names := writeFixtures(t, map[string]string{
		"a.md":           "Run it from C:\\" + users + "\\" + someone + "\\src.\n",
		"b.go":           "package b\n\nvar p = `c:/" + strings.ToLower(users) + "/" + strings.ToUpper(someone) + "/x`\n",
		"c.txt":          "E:\\" + work + "\\demo\n",
		"d.md":           "Tags are " + strings.ToUpper(phrase[:1]) + phrase[1:] + "'s to create.\n",
		"e.go":           "package e\n\n// Left for " + phrase + " to decide.\nfunc E() {}\n",
		"f.json":         `{"path":"D:\\\\` + users + `\\\\` + someone + `\\\\x"}` + "\n",
		"internal/h.go":  "package h\n\n// A comment.\nvar s = \"left for " + phrase + "\"\n",
		"i.txt":          "Measured for " + phrase + " in a text file.\n",
		"placeholder.md": "C:\\" + users + "\\dev\\x, C:\\" + users + "\\%USERNAME%\\x, C:\\" + users + "\\<name>\\x, c:/" + users + "/me/x\n",
		"wrap.md":        "Tags are for the\n  owner" + " to create.\n",
		"wrap.go":        "package w\n\n// Left for the\n// owner" + " to decide.\nfunc W() {}\n",
		"block.go":       "package k\n\n/* Left for " + phrase + ". */\nfunc K() {}\n",
		"apart.go":       "package a\n\n// Left for the\nfunc A() {}\n\n// owner" + " of the lock.\nfunc B() {}\n",
		"msys.sh":        "cd /c/" + users + "/" + someone + "/src\n",
		"wsl.sh":         "ls \"/mnt/c/" + users + "/" + someone + "\"\n",
		"url.md":         "See https://example.com/a/" + strings.ToLower(users) + "/" + someone + " and /c/" + users + "/dev.\n",
	})
	r, err := check(root, append(names, "gone.md"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"a.md":   "user-folder;",
		"b.go":   "user-folder;",
		"c.txt":  "local-work-path;",
		"d.md":   "narrates-a-person;",
		"e.go":   "narrates-a-person;",
		"f.json": "user-folder;",
		// The words may be split by a line wrap or across comment lines that follow each other.
		"wrap.md":  "narrates-a-person;",
		"wrap.go":  "narrates-a-person;",
		"block.go": "narrates-a-person;",
		"msys.sh":  "user-folder;",
		"wsl.sh":   "user-folder;",
	}
	got := rulesOf(r)
	if len(got) != len(want) {
		t.Errorf("findings %v; want %v", got, want)
	}
	for file, rules := range want {
		if got[file] != rules {
			t.Errorf("%s: %q; want %q", file, got[file], rules)
		}
	}
	if r.Files != len(names) {
		t.Errorf("files = %d; want %d (a missing file is skipped)", r.Files, len(names))
	}
}

func TestBinaryFilesAreSkippedAndFindingsAreCapped(t *testing.T) {
	var lines strings.Builder
	for i := 0; i < maxFindings+5; i++ {
		lines.WriteString(work + "\n")
	}
	root, names := writeFixtures(t, map[string]string{
		"many.txt": lines.String(),
		"pic.png":  "\x00" + work,
	})
	r, err := check(root, names)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Findings) != maxFindings || !r.Truncated || r.Files != 1 {
		t.Errorf("findings %d, truncated %v, files %d; want %d, true, 1", len(r.Findings), r.Truncated, r.Files, maxFindings)
	}
}

func gitFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root, _ := writeFixtures(t, files)
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "tracked.md"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = root, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return root
}

// Only tracked files count, the report is one JSON object, and a finding fails the run.
func TestTheRunReadsTrackedFilesAndFailsOnAFinding(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--root"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), `"error"`) {
		t.Errorf("a missing root: code %d, stderr %q; want 2 and an error object", code, stderr.String())
	}
	stderr.Reset()
	if code := run([]string{"--root", t.TempDir()}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), `"error"`) {
		t.Errorf("a root outside git: code %d, stderr %q; want 2 and an error object", code, stderr.String())
	}
	for _, tc := range []struct {
		content string
		code    int
	}{{"fine\n", 0}, {"see " + work + "\n", 1}} {
		stdout.Reset()
		root := gitFixture(t, map[string]string{"tracked.md": tc.content, "untracked.md": work + "\n"})
		code := run([]string{"--root", root}, &stdout, &stderr)
		var r report
		if err := json.Unmarshal(stdout.Bytes(), &r); err != nil || code != tc.code || r.Files != 1 || len(r.Findings) != tc.code {
			t.Errorf("%q: code %d, report %s (%v); want code %d over one file", tc.content, code, stdout.String(), err, tc.code)
		}
	}
}
