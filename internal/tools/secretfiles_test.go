package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretFileKnowsTheSecretNamesAndOnlyThem(t *testing.T) {
	for path, want := range map[string]bool{
		".env":                      true,
		".env.local":                true,
		".env.staging":              true,
		"web/.env.production":       true,
		".envrc":                    true,
		".env.example":              true, // refused too: a template costs one turn
		`site\.ENV`:                 true, // Windows separators and case
		"worker/.dev.vars":          true,
		"certs/server.pem":          true,
		"keys/signing.key":          true,
		".git-credentials":          true,
		".private/notes.md":         true,
		"a/.private/b/c.txt":        true,
		"home/.ssh/id_ed25519":      true,
		".aws/credentials":          true,
		".kube/config":              true,
		".wrangler/config/default":  true,
		".env.":                     true, // Windows drops trailing dots and spaces
		"certs/x.pem ":              true,
		".env::$DATA":               true, // and reads a stream name as the file
		`C:\Users\me\.ssh\config`:   true,
		"release/upload.keystore":   true,
		"certs/client.p12":          true,
		".netrc":                    true,
		"_netrc":                    true,
		".pypirc":                   true,
		"../outside/.env":           true,
		"src/env.go":                false,
		"src/key.go":                false,
		"docs/keys.md":              false,
		"internal/private/x.go":     false, // private without the dot is code
		"testdata/environment.json": false,
		"README.md":                 false,
	} {
		if got := SecretFile(path); got != want {
			t.Errorf("SecretFile(%q) = %v, want %v", path, got, want)
		}
	}
}

// The api backend's file tools refuse a secret file inside the workspace,
// before an approval is asked for and again if Run is called directly, and
// a search of a directory passes over them. --allow-dangerous lifts it.
func TestTheFileToolsRefuseSecretFiles(t *testing.T) {
	ws, tl := setup(t)
	must(t, os.WriteFile(filepath.Join(ws.Root, ".env"), []byte("API_TOKEN=hunter2-from-dotenv\n"), 0o644))
	must(t, os.MkdirAll(filepath.Join(ws.Root, ".private"), 0o755))
	must(t, os.WriteFile(filepath.Join(ws.Root, ".private", "plan.md"), []byte("hunter2-private\n"), 0o644))

	calls := []struct {
		tool string
		in   map[string]any
	}{
		{"read_file", map[string]any{"path": ".env"}},
		{"read_file", map[string]any{"path": ".private/plan.md"}},
		{"write_file", map[string]any{"path": ".env.production", "content": "X=1"}},
		{"write_file", map[string]any{"path": "certs/new.pem", "content": "-----BEGIN"}},
		{"edit_file", map[string]any{"path": ".env", "old_string": "hunter2", "new_string": "x"}},
		{"grep", map[string]any{"pattern": "hunter2", "path": ".private"}},
		{"grep", map[string]any{"pattern": "hunter2", "path": ".env"}}, // one file, not a walk
		{"read_file", map[string]any{"path": ".env."}},                 // Windows' other spelling
	}
	for _, c := range calls {
		raw, _ := json.Marshal(c.in)
		if rule := tl[c.tool].Refuse(ws, raw); rule != "secret-file" {
			t.Errorf("%s %v: Refuse = %q, want secret-file", c.tool, c.in, rule)
		}
		if out, err := call(t, ws, tl[c.tool], c.in); err == nil || !strings.Contains(err.Error(), "guard (secret-file)") {
			t.Errorf("%s %v run directly: out=%q err=%v; want the guard's refusal", c.tool, c.in, out, err)
		}
	}
	if _, err := os.Stat(filepath.Join(ws.Root, ".env.production")); err == nil {
		t.Error("a refused write_file created the file")
	}

	// A search of the whole workspace runs, and skips the secret files.
	out, err := call(t, ws, tl["grep"], map[string]any{"pattern": "hunter2"})
	if err != nil || out != "(no matches)" {
		t.Errorf("grep over the workspace = %q, %v; want the secret files passed over", out, err)
	}
	// Ordinary files are untouched by the rule, one file or a directory.
	if out, err := call(t, ws, tl["read_file"], map[string]any{"path": "src/a.go"}); err != nil || !strings.Contains(out, "Hello") {
		t.Errorf("read_file src/a.go = %q, %v", out, err)
	}
	if out, err := call(t, ws, tl["grep"], map[string]any{"pattern": "Hello", "path": "src/a.go"}); err != nil || !strings.Contains(out, "a.go:3:") {
		t.Errorf("grep src/a.go = %q, %v", out, err)
	}

	ws.AllowDangerous = true
	for _, c := range calls[:2] {
		raw, _ := json.Marshal(c.in)
		if rule := tl[c.tool].Refuse(ws, raw); rule != "" {
			t.Errorf("allow_dangerous: %s Refuse = %q, want none", c.tool, rule)
		}
	}
	if out, err := call(t, ws, tl["grep"], map[string]any{"pattern": "hunter2"}); err != nil || !strings.Contains(out, ".env:1:") {
		t.Errorf("allow_dangerous: grep = %q, %v; want the .env match", out, err)
	}
}

// A link is judged by where it leads: notes.txt -> .env is the .env file, to
// the file tools and to grep's walk, and a link out of the workspace is not
// followed by the walk even with the guard lifted.
func TestALinkToASecretFileIsTheSecretFile(t *testing.T) {
	ws, tl := setup(t)
	must(t, os.WriteFile(filepath.Join(ws.Root, ".env"), []byte("hunter2-linked\n"), 0o644))
	outside := filepath.Join(t.TempDir(), "elsewhere.txt")
	must(t, os.WriteFile(outside, []byte("hunter2-outside\n"), 0o644))
	if err := os.Symlink(filepath.Join(ws.Root, ".env"), filepath.Join(ws.Root, "notes.txt")); err != nil {
		t.Skipf("cannot make a symlink here: %v", err) // Windows without the privilege
	}
	must(t, os.Symlink(outside, filepath.Join(ws.Root, "src", "away.txt")))

	raw, _ := json.Marshal(map[string]any{"path": "notes.txt"})
	if rule := tl["read_file"].Refuse(ws, raw); rule != "secret-file" {
		t.Errorf("read_file notes.txt -> .env: Refuse = %q, want secret-file", rule)
	}
	if out, err := call(t, ws, tl["grep"], map[string]any{"pattern": "hunter2"}); err != nil || out != "(no matches)" {
		t.Errorf("grep = %q, %v; want the link to .env and the link out both passed over", out, err)
	}
	ws.AllowDangerous = true
	out, err := call(t, ws, tl["grep"], map[string]any{"pattern": "hunter2"})
	if err != nil || !strings.Contains(out, "hunter2-linked") || strings.Contains(out, "hunter2-outside") {
		t.Errorf("allow_dangerous: grep = %q, %v; want the .env match and still nothing from outside", out, err)
	}
}

// A workspace that itself sits under a secret directory is judged by the
// paths inside it, or nothing in it could be read.
func TestAWorkspaceInsideASecretDirectoryIsNotRefusedAsAWhole(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".private", "repo")
	must(t, os.MkdirAll(dir, 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644))
	ws, err := NewWorkspace(dir)
	must(t, err)
	raw, _ := json.Marshal(map[string]any{"path": "main.go"})
	if rule := refuseSecretFile(ws, raw); rule != "" {
		t.Errorf("main.go in a workspace under .private: Refuse = %q, want none", rule)
	}
}
