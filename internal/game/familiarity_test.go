package game

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestFamiliarityEvidence(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		b, e := c.CombinedOutput()
		if e != nil {
			t.Fatalf("git %v: %v: %s", args, e, b)
		}
		return string(b)
	}
	git("init")
	git("config", "user.name", "Person")
	git("config", "user.email", "person@example.test")
	commit := func(file, date string) string {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(file))
		if e := os.MkdirAll(filepath.Dir(p), 0755); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(p, []byte(file), 0644); e != nil {
			t.Fatal(e)
		}
		git("add", file)
		c := exec.Command("git", "-C", dir, "commit", "-m", file)
		c.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
		if b, e := c.CombinedOutput(); e != nil {
			t.Fatalf("commit: %v %s", e, b)
		}
		return git("rev-parse", "HEAD")
	}
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	commit("old/file", "2026-01-01T12:00:00Z")
	commit("known/file", "2026-10-29T12:00:00Z")
	runHash := commit("a/person", "2026-10-29T12:30:00Z")
	runEnd := commit("runonly/file", "2026-10-29T12:31:00Z")
	base := runEnd
	git("checkout", "-b", "upstream")
	commit("upstream/file", "2026-10-29T12:35:00Z")
	git("checkout", "-b", "hrn/task", base[:40])
	commit("batchmiddle/file", "2026-10-29T12:40:00Z")
	commit("internal/new/file.go", "2026-10-29T13:00:00Z")
	git("merge", "--no-ff", "upstream", "-m", "merge upstream")
	hash := git("rev-parse", "HEAD")
	b := batch.Result{Dir: dir, Base: base[:40], Tasks: []batch.TaskResult{{Branch: "hrn/task", Commit: hash[:40]}}}
	r := runlog.Record{Task: agent.Task{Dir: dir}, Result: agent.Result{ChangedFiles: []string{"standalone/file", "a/file"}, Commits: []string{runHash[:40], runEnd[:40]}}}
	got, err := deriveFamiliarity(context.Background(), Records{Batches: []batch.Result{b}, Runs: []runlog.Record{r}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Known) != 2 || got.Known[0].Area != "known" || got.Known[1].Area != "upstream" {
		t.Fatalf("known: %+v", got.Known)
	}
	if len(got.Unseen) != 4 || got.Unseen[0].Area != "a" || got.Unseen[1].Area != "batchmiddle" || got.Unseen[2].Area != "internal/new" || got.Unseen[3].Area != "standalone" {
		t.Fatalf("unseen: %+v", got.Unseen)
	}
}

func TestFamiliarityDoesNotVerifySignatures(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.test")
	git("commit", "--allow-empty", "-m", "base")
	base := git("rev-parse", "HEAD")
	tree := git("rev-parse", "HEAD^{tree}")
	commit := "tree " + tree + "\nparent " + base + "\nauthor Test <test@example.test> 1793354400 +0000\ncommitter Test <test@example.test> 1793354400 +0000\ngpgsig -----BEGIN PGP SIGNATURE-----\n fake\n -----END PGP SIGNATURE-----\n\nsigned\n"
	cmd := exec.Command("git", "-C", dir, "hash-object", "-t", "commit", "-w", "--stdin")
	cmd.Stdin = strings.NewReader(commit)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hash-object: %v: %s", err, out)
	}
	head := strings.TrimSpace(string(out))
	git("update-ref", "HEAD", head)
	marker := filepath.ToSlash(filepath.Join(t.TempDir(), "gpg-ran"))
	script := filepath.Join(t.TempDir(), "gpg.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho ran > '"+marker+"'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	git("config", "log.showSignature", "true")
	git("config", "gpg.program", filepath.ToSlash(script))
	records := Records{Runs: []runlog.Record{{Task: agent.Task{Dir: dir}}}, Batches: []batch.Result{{Dir: dir, Base: base, Tasks: []batch.TaskResult{{Branch: "hrn/task", Commit: head}}}}}
	if _, err := deriveFamiliarity(context.Background(), records, time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("gpg marker: %v", err)
	}
}
func TestFamiliarityPracticeAndMissingRepo(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := filepath.Join(Dir(), "reps")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data := `{"date":"2026-10-30","daily":{"kind":"cold read","source":"owner/repo#1","material":"--- a/internal/alpha/a.go\n+++ b/internal/alpha/a.go\n","answered_at":"2026-10-30T12:00:00Z"},"reads":[{"kind":"read","source":"owner/repo#2","material":"+++ b/internal/alpha/b.go\n+++ b/cmd/tool/main.go\n","answered_at":"2026-10-30T12:00:00Z"},{"kind":"read","source":"owner/repo#3","material":"+++ b/ignored/file"}]}`
	if err := os.WriteFile(filepath.Join(dir, "2026-10-30.json"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "2000-01-01.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := deriveFamiliarity(context.Background(), Records{Batches: []batch.Result{{Dir: filepath.Join(t.TempDir(), "missing")}}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !got.UnreadablePractice || len(got.Known) != 2 || got.Known[0].Repo != "owner/repo" || got.Known[0].Area != "internal/alpha" || got.Known[0].Count != 2 || got.Known[1].Area != "cmd/tool" {
		t.Fatalf("known: %+v", got.Known)
	}
}

func TestFileAreaPreservesGitPaths(t *testing.T) {
	for _, name := range []string{"a/file", "b/file"} {
		if got := fileArea(name); got != name[:1] {
			t.Errorf("fileArea(%q) = %q", name, got)
		}
	}
}

func TestGitEvidenceIgnoresRepositoryOverrides(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("git", "-C", dir, "init")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init: %v: %s", err, out)
	}
	t.Setenv("GIT_DIR", filepath.Join(dir, "missing"))
	if got := gitEvidence(context.Background(), dir, "rev-parse", "--is-inside-work-tree"); got != "true\n" {
		t.Fatalf("got %q", got)
	}
}
