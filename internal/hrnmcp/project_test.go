package hrnmcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
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
	for _, args := range []string{`{"backend":"claude"}`, `{"model":"other"}`, `{"dir":"elsewhere"}`, `{"retrieval_project":"other"}`, `{"retrieval_version":"other"}`, `{"continue_run_id":"prior"}`} {
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
	srv, err := NewProjectServer(dir, path)
	if err != nil {
		t.Fatal(err)
	}
	s := connectTo(t, srv)
	names := map[string]bool{}
	for _, tool := range s.cl.Tools() {
		names[tool.Name] = true
	}
	if len(names) != 4 || !names["run"] || !names["runs"] || !names["show"] || !names["backends"] {
		t.Fatalf("project tools: %v", names)
	}
	got := s.call(t, "run", `{"prompt":"baseline","retrieval":"off"}`)
	if got.IsError {
		t.Fatal(got)
	}
}
