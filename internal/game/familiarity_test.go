package game

import (
	"os"
	"os/exec"
	"path/filepath"
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
	git("checkout", "-b", "hrn/task")
	hash := commit("internal/new/file.go", "2026-10-29T13:00:00Z")
	b := batch.Result{Dir: dir, Tasks: []batch.TaskResult{{Branch: "hrn/task", Commit: hash[:40]}}}
	r := runlog.Record{Task: agent.Task{Dir: dir}, Result: agent.Result{ChangedFiles: []string{"standalone/file", "a/file"}}}
	got, err := deriveFamiliarity(Records{Batches: []batch.Result{b}, Runs: []runlog.Record{r}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Known) != 1 || got.Known[0].Area != "known" {
		t.Fatalf("known: %+v", got.Known)
	}
	if len(got.Unseen) != 3 || got.Unseen[0].Area != "a" || got.Unseen[1].Area != "internal/new" || got.Unseen[2].Area != "standalone" {
		t.Fatalf("unseen: %+v", got.Unseen)
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
	got, err := deriveFamiliarity(Records{Batches: []batch.Result{{Dir: filepath.Join(t.TempDir(), "missing")}}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Known) != 2 || got.Known[0].Repo != "owner/repo" || got.Known[0].Area != "internal/alpha" || got.Known[0].Count != 2 || got.Known[1].Area != "cmd/tool" {
		t.Fatalf("known: %+v", got.Known)
	}
}
