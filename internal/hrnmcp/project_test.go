package hrnmcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/prefs"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestProjectProfileScopeAndFreshness(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	dir := t.TempDir()
	source := []byte("reviewed source\n")
	if err := os.WriteFile(filepath.Join(dir, "source.txt"), source, 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init"}, {"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "baseline"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", out, err)
		}
	}
	head, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(source)
	p := ProjectProfile{Workspace: dir, Backend: "codex", Options: agent.Options{Model: "test-model", Retrieval: "required", RetrievalTenant: "tenant", RetrievalProject: "project", RetrievalRevision: string(head[:40]), RetrievalVersion: "version"}, SourceHashes: map[string]string{"source.txt": hex.EncodeToString(sum[:])}}
	raw, err := p.arguments(context.Background(), dir, json.RawMessage(`{"prompt":"task"}`))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["retrieval"] != "required" || got["model"] != "test-model" || got["backend"] != "codex" || got["mode"] != nil {
		t.Fatalf("bad defaults: %s", raw)
	}
	for _, args := range []string{`{"backend":"claude"}`, `{"model":"other"}`, `{"dir":"elsewhere"}`, `{"retrieval_project":"other"}`, `{"retrieval_version":"other"}`, `{"continue_run_id":"prior"}`, `{"fresh":true}`, `{"retrieval_refresh":true}`} {
		if _, err := p.arguments(context.Background(), dir, json.RawMessage(args)); err == nil {
			t.Fatalf("accepted mismatched scope: %s", args)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "source.txt"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.arguments(context.Background(), dir, json.RawMessage(`{"prompt":"task"}`)); err == nil {
		t.Fatal("accepted stale reviewed source")
	}
	raw, err = p.arguments(context.Background(), dir, json.RawMessage(`{"prompt":"baseline","retrieval":"off"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &got); err != nil || got["retrieval"] != "off" {
		t.Fatalf("explicit off lost: %s %v", raw, err)
	}
	if _, err := p.arguments(context.Background(), t.TempDir(), json.RawMessage(`{"prompt":"task"}`)); err == nil {
		t.Fatal("accepted different server workspace")
	}
	if err := os.WriteFile(filepath.Join(dir, "source.txt"), source, 0600); err != nil {
		t.Fatal(err)
	}
	p.Options.RetrievalRevision = "0000000000000000000000000000000000000000"
	if _, err := p.arguments(context.Background(), dir, json.RawMessage(`{"prompt":"task"}`)); err == nil {
		t.Fatal("accepted changed revision")
	}
}

func TestProjectServerExposesOnlyScopedRunAndInspectionTools(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	dir := t.TempDir()
	source := []byte("reviewed source\n")
	if err := os.WriteFile(filepath.Join(dir, "source.txt"), source, 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init"}, {"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "baseline"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", out, err)
		}
	}
	head, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(source)
	p := ProjectProfile{Workspace: dir, Backend: "stub", Options: agent.Options{Model: "stub-1", Retrieval: "required", RetrievalTenant: "tenant", RetrievalProject: "project", RetrievalRevision: string(head[:40]), RetrievalVersion: "version"}, SourceHashes: map[string]string{"source.txt": hex.EncodeToString(sum[:])}}
	path := filepath.Join(t.TempDir(), "profile.json")
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body, want string
	}{
		{"oversized", string(data) + strings.Repeat(" ", 8<<20), "8 MiB"},
		{"unknown token", string(data[:len(data)-1]) + `,"token":"fixture"}`, "unknown field"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			badPath := filepath.Join(t.TempDir(), "profile.json")
			if err := os.WriteFile(badPath, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := NewProjectServer(dir, badPath); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("profile accepted or wrong error: %v", err)
			}
		})
	}
	srv, err := NewProjectServer(dir, path)
	if err != nil {
		t.Fatal(err)
	}
	s := connectTo(t, srv)
	names := map[string]bool{}
	for _, tool := range s.cl.Tools() {
		names[tool.Name] = true
		if tool.Name == "run" {
			var schema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
				t.Fatal(err)
			}
			if _, ok := schema.Properties["retrieval_refresh"]; ok {
				t.Fatal("project run advertises retrieval_refresh")
			}
		}
	}
	if len(names) != 4 || !names["run"] || !names["runs"] || !names["show"] || !names["backends"] {
		t.Fatalf("project tools: %v", names)
	}
	got := s.call(t, "run", `{"prompt":"baseline","retrieval":"off"}`)
	if got.IsError {
		t.Fatal(got)
	}
	t.Run("disabled profile backend is refused before advertising it", func(t *testing.T) {
		t.Setenv("HRN_HOME", t.TempDir())
		if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: []string{"claude"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := NewProjectServer(dir, path); err == nil || !strings.Contains(err.Error(), "backend stub is disabled") {
			t.Fatalf("disabled profile: %v", err)
		}
	})
}

func TestTheProfileCheckReadsItsOwnWorkspacesHead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	dir, other := t.TempDir(), t.TempDir()
	for i, d := range []string{dir, other} {
		for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-q", "--allow-empty", "-m", "c" + string(rune('0'+i))}} {
			if out, err := exec.Command("git", append([]string{"-C", d}, args...)...).CombinedOutput(); err != nil {
				t.Fatalf("git: %s %v", out, err)
			}
		}
	}
	head, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source.txt"), []byte("s"), 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("s"))
	p := ProjectProfile{Workspace: dir, Backend: "codex", Options: agent.Options{Model: "test-model", Retrieval: "required", RetrievalTenant: "t", RetrievalProject: "p",
		RetrievalRevision: strings.TrimSpace(string(head)), RetrievalVersion: "v"}, SourceHashes: map[string]string{"source.txt": hex.EncodeToString(sum[:])}}
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	if err := p.validate(context.Background(), dir); err != nil {
		t.Errorf("the check read the HEAD of the repository GIT_DIR named: %v", err)
	}
}
