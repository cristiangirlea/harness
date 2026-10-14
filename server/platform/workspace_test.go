package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func workspaceJob(id string) Job {
	return Job{
		ID: "j_" + id, TenantID: "t_" + strings.Repeat("a", 32),
		ProjectID: "p_" + strings.Repeat("b", 32),
		BaseRef:   strings.Repeat("c", 40), RepositoryURL: "https://example.invalid/repo.git",
		Backend: "api", Prompt: "inspect",
	}
}

func TestWorkspaceRetainsFilesAndRejectsIdentityDrift(t *testing.T) {
	root := t.TempDir()
	job := workspaceJob(strings.Repeat("d", 32))
	dir, err := prepareWorkspace(root, job, false)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "untracked.txt")
	if err := os.WriteFile(marker, []byte("retained across restart"), 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := prepareWorkspace(root, job, true)
	if err != nil || again != dir {
		t.Fatalf("resume workspace %q: %v", again, err)
	}
	if data, err := os.ReadFile(filepath.Join(again, "untracked.txt")); err != nil || string(data) != "retained across restart" {
		t.Fatalf("untracked file: %q %v", data, err)
	}
	other := workspaceJob(strings.Repeat("e", 32))
	otherDir, err := prepareWorkspace(root, other, false)
	if err != nil || otherDir == dir {
		t.Fatalf("separate job workspace %q: %v", otherDir, err)
	}
	if _, err := os.Stat(filepath.Join(otherDir, "untracked.txt")); !os.IsNotExist(err) {
		t.Fatalf("other job saw untracked file: %v", err)
	}
	changed := job
	changed.BaseRef = strings.Repeat("f", 40)
	if _, err := prepareWorkspace(root, changed, true); err == nil {
		t.Fatal("different base commit reused existing workspace")
	}
	changed = job
	changed.RepositoryURL = "https://example.invalid/other.git"
	if _, err := prepareWorkspace(root, changed, true); err == nil {
		t.Fatal("different repository reused existing workspace")
	}
	changed = job
	changed.TenantID = "t_" + strings.Repeat("f", 32)
	if _, err := prepareWorkspace(root, changed, true); err == nil {
		t.Fatal("different tenant reused existing workspace")
	}
	changed = job
	changed.Prompt = "different task"
	if _, err := prepareWorkspace(root, changed, true); err == nil {
		t.Fatal("different prompt reused existing workspace")
	}
	changed = job
	changed.Options.MaxTurns = 9
	if _, err := prepareWorkspace(root, changed, true); err == nil {
		t.Fatal("different options reused existing workspace")
	}
}

func TestWorkspaceRejectsInvalidRepositoryIdentity(t *testing.T) {
	job := workspaceJob(strings.Repeat("d", 32))
	for _, repositoryURL := range []string{"", "file:///tmp/repo", "https://user:pass@example.invalid/repo.git"} {
		job.RepositoryURL = repositoryURL
		if _, err := prepareWorkspace(t.TempDir(), job, false); err == nil {
			t.Fatalf("accepted invalid repository URL %q", repositoryURL)
		}
	}
}

func TestWorkspaceRejectsLegacyManifestWithoutDeletingFiles(t *testing.T) {
	root := t.TempDir()
	job := workspaceJob(strings.Repeat("d", 32))
	dir, err := prepareWorkspace(root, job, false)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "untracked.txt")
	if err := os.WriteFile(marker, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(root, ".hrn-control", job.ID+".json")
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `"version":2`, `"version":1`, 1))
	if err := os.WriteFile(manifest, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareWorkspace(root, job, true); err == nil {
		t.Fatal("legacy manifest was silently reused")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "preserve" {
		t.Fatalf("untracked file changed: %q %v", data, err)
	}
}

func TestWorkspaceRejectsMissingOrDamagedManifest(t *testing.T) {
	root := t.TempDir()
	job := workspaceJob(strings.Repeat("d", 32))
	if _, err := prepareWorkspace(root, job, false); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(root, ".hrn-control", job.ID+".json")
	if err := os.Remove(manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareWorkspace(root, job, true); err == nil {
		t.Fatal("checkpoint resumed without manifest")
	}
	if _, err := prepareWorkspace(root, job, false); err == nil {
		t.Fatal("orphaned workspace was silently reused")
	}
	if err := os.WriteFile(manifest, []byte("{truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareWorkspace(root, job, true); err == nil {
		t.Fatal("corrupt manifest was accepted")
	}
}

func TestWorkspaceRejectsSymlinksAndMissingRoot(t *testing.T) {
	job := workspaceJob(strings.Repeat("d", 32))
	if _, err := prepareWorkspace(filepath.Join(t.TempDir(), "missing"), job, false); err == nil {
		t.Fatal("worker created a missing volume root")
	}
	root := t.TempDir()
	external := t.TempDir()
	if err := os.Symlink(external, filepath.Join(root, "workspaces")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := prepareWorkspace(root, job, false); err == nil {
		t.Fatal("symlinked workspace root was accepted")
	}
	root = t.TempDir()
	dir, err := prepareWorkspace(root, job, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareWorkspace(root, job, true); err == nil {
		t.Fatal("symlinked job workspace was accepted")
	}
}
