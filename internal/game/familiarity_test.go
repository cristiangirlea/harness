package game

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
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

func familiarityRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init"}, {"config", "user.name", "Person"}, {"config", "user.email", "person@example.test"}, {"commit", "--allow-empty", "-m", "base"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func stubGit(t *testing.T, fn func(context.Context, *exec.Cmd) ([]byte, error)) {
	t.Helper()
	old := runGit
	runGit = fn
	t.Cleanup(func() { runGit = old })
}

func hangingGit(ctx context.Context, _ *exec.Cmd) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestFamiliarityLeavesOutFoldersOutsideRepositories(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	var calls atomic.Int32
	stubGit(t, func(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
		calls.Add(1)
		return cmd.Output()
	})
	plain := t.TempDir()
	runs := []runlog.Record{
		{Task: agent.Task{Dir: plain}, Result: agent.Result{ChangedFiles: []string{"answer.txt"}}},
		{Task: agent.Task{Dir: filepath.Join(plain, "gone")}, Result: agent.Result{ChangedFiles: []string{"answer.txt"}}},
	}
	got, err := deriveFamiliarity(context.Background(), Records{Runs: runs, Batches: []batch.Result{{Dir: plain}}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Unseen) != 0 || len(got.Known) != 0 || got.Partial || calls.Load() != 0 {
		t.Fatalf("got %+v after %d git calls", got, calls.Load())
	}
}

func TestFamiliarityStopsAtItsDeadline(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	stubGit(t, hangingGit)
	var runs []runlog.Record
	for range 12 {
		runs = append(runs, runlog.Record{Task: agent.Task{Dir: familiarityRepo(t)}, Result: agent.Result{ChangedFiles: []string{"area/file"}}})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	got, err := deriveFamiliarity(ctx, Records{Runs: runs}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("took %v", elapsed)
	}
	if !got.Partial || len(got.Unseen) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestReadFamiliarityKeepsToItsBudget(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	old := familiarityBudget
	familiarityBudget = 200 * time.Millisecond
	t.Cleanup(func() { familiarityBudget = old })
	stubGit(t, hangingGit)
	for range 3 {
		w, err := runlog.Create("fake", agent.Task{Dir: familiarityRepo(t)}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Close(agent.Result{ChangedFiles: []string{"area/file"}}); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	got, err := ReadFamiliarity(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second || !got.Partial {
		t.Fatalf("got %+v after %v", got, elapsed)
	}
}

func TestFamiliarityKeepsGitReads(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := familiarityRepo(t)
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(file string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(file), 0644); err != nil {
			t.Fatal(err)
		}
		git("add", file)
		git("commit", "-m", file)
	}
	write("personarea/f")
	base := git("rev-parse", "HEAD")
	git("checkout", "-b", "hrn/task")
	write("middlearea/f")
	write("agentarea/f")
	head := git("rev-parse", "HEAD")
	records := Records{Batches: []batch.Result{{Dir: dir, Base: base, Tasks: []batch.TaskResult{{Branch: "hrn/task", Commit: head}}}}}
	now := time.Now()
	check := func(name string, got Familiarity, partial bool) {
		t.Helper()
		if partial {
			if !got.Partial || len(got.Known) != 0 || len(got.Unseen) != 0 {
				t.Fatalf("%s: %+v", name, got)
			}
			return
		}
		if got.Partial || len(got.Known) != 1 || got.Known[0].Area != "personarea" || len(got.Unseen) != 2 || got.Unseen[0].Area != "agentarea" || got.Unseen[1].Area != "middlearea" {
			t.Fatalf("%s: %+v", name, got)
		}
	}
	first, err := deriveFamiliarity(context.Background(), records, now)
	if err != nil {
		t.Fatal(err)
	}
	check("first", first, false)
	var calls atomic.Int32
	stubGit(t, func(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
		calls.Add(1)
		return cmd.Output()
	})
	second, err := deriveFamiliarity(context.Background(), records, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	check("second", second, false)
	if calls.Load() != 0 {
		t.Fatalf("second read ran git %d times", calls.Load())
	}
	stubGit(t, hangingGit)
	hung := func(at time.Time) Familiarity {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		got, err := deriveFamiliarity(ctx, records, at)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	check("stale", hung(now.Add(time.Hour)), false)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	warm, err := deriveFamiliarity(cancelled, records, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	check("warm after the deadline", warm, false)
	check("too old", hung(now.Add(25*time.Hour)), true)
	kept := loadFamiliarityCache()
	if len(kept.Batches) != 1 || len(kept.Repos) != 1 {
		t.Fatalf("a timed-out read dropped kept reads: %+v", kept)
	}
	if _, err := deriveFamiliarity(context.Background(), Records{}, now); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(Dir(), "familiarity-cache.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), head) || strings.Contains(string(data), "personarea") {
		t.Fatalf("unused reads kept: %s", data)
	}
}

func TestFamiliarityLeavesOutGitEvidenceOfAHalfReadRepository(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := familiarityRepo(t)
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	base := git("rev-parse", "HEAD")
	git("checkout", "-b", "hrn/task")
	if err := os.WriteFile(filepath.Join(dir, "agentfile"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "agent under the person's identity")
	head := git("rev-parse", "HEAD")
	now := time.Now()
	runs := []runlog.Record{{Task: agent.Task{Dir: dir}}}
	if _, err := deriveFamiliarity(context.Background(), Records{Runs: runs}, now); err != nil {
		t.Fatal(err)
	}
	name := repoName(dir)
	reps := filepath.Join(Dir(), "reps")
	if err := os.MkdirAll(reps, 0700); err != nil {
		t.Fatal(err)
	}
	rep := `{"date":"2026-10-30","daily":{"kind":"read","source":"` + name + `#1","material":"+++ b/docs/a.md\n","answered_at":"2026-10-30T12:00:00Z"}}`
	if err := os.WriteFile(filepath.Join(reps, "2026-10-30.json"), []byte(rep), 0600); err != nil {
		t.Fatal(err)
	}
	stubGit(t, hangingGit)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	records := Records{Runs: runs, Batches: []batch.Result{{Dir: dir, Base: base, Tasks: []batch.TaskResult{{Branch: "hrn/task", Commit: head}}}}}
	got, err := deriveFamiliarity(ctx, records, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Partial || len(got.Unseen) != 0 || len(got.Known) != 1 || got.Known[0].Area != "docs" {
		t.Fatalf("got %+v", got)
	}
	data, err := os.ReadFile(filepath.Join(Dir(), "familiarity-cache.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "agentfile") {
		t.Fatalf("cached repository read dropped: %s", data)
	}
}

func TestFamiliarityTakesARepositoryWithoutIdentityAsRead(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := familiarityRepo(t)
	if out, err := exec.Command("git", "-C", dir, "config", "--unset", "user.email").CombinedOutput(); err != nil {
		t.Fatalf("unset: %v: %s", err, out)
	}
	runs := []runlog.Record{{Task: agent.Task{Dir: dir}, Result: agent.Result{ChangedFiles: []string{"area/file"}}}}
	got, err := deriveFamiliarity(context.Background(), Records{Runs: runs}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got.Partial || len(got.Unseen) != 1 || got.Unseen[0].Area != "area" {
		t.Fatalf("got %+v", got)
	}
}

func TestFamiliarityGitReadsDoNotGrowWithRecords(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	var calls atomic.Int32
	stubGit(t, func(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
		calls.Add(1)
		return cmd.Output()
	})
	dir := familiarityRepo(t)
	gone := t.TempDir()
	var runs []runlog.Record
	for i := range 250 {
		d := dir
		if i%5 == 0 {
			d = filepath.Join(gone, "eval", strings.Repeat("r", i%7+1), "work")
		}
		runs = append(runs, runlog.Record{Task: agent.Task{Dir: d}, Result: agent.Result{ChangedFiles: []string{"area/file"}}})
	}
	if _, err := deriveFamiliarity(context.Background(), Records{Runs: runs}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("%d git calls for 250 records in one repository", n)
	}
}

func TestFamiliarityCacheRejectsWhatItCannotTrust(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := familiarityRepo(t)
	if err := os.MkdirAll(Dir(), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(Dir(), "familiarity-cache.json")
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	runs := []runlog.Record{{Task: agent.Task{Dir: dir}}}
	records := Records{Runs: runs, Batches: []batch.Result{{Dir: dir, Base: "HEAD", Tasks: []batch.TaskResult{{Branch: "hrn/task", Commit: "main"}}}}}
	if _, err := deriveFamiliarity(context.Background(), records, now); err != nil {
		t.Fatal(err)
	}
	c := loadFamiliarityCache()
	if len(c.Batches) != 0 || len(c.Repos) != 1 {
		t.Fatalf("cache: %+v", c)
	}
	root, _ := filepath.Abs(dir)
	c.Repos[root] = repoEvidence{Checked: now.Add(time.Hour), Commits: []authored{{Hash: strings.Repeat("a", 40), At: now.Unix(), Areas: []string{"invented"}}}}
	saveFamiliarityCache(c)
	got, err := deriveFamiliarity(context.Background(), Records{Runs: runs}, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Partial || len(got.Known) != 0 {
		t.Fatalf("future-dated read was trusted: %+v", got)
	}
}
