package workmap

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func TestBuildReadsLocalPlanAndBranches(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "none"))
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}
	git("init", "-b", "main")
	git("config", "user.email", "test@example.invalid")
	git("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "PLAN.md"), []byte("## What is open\n\n### Work\n- [ ] An idea\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "PLAN.md")
	git("commit", "-m", "initial")
	git("checkout", "-b", "feature")
	git("commit", "--allow-empty", "-m", "feature")
	git("checkout", "main")
	git("merge", "--no-ff", "feature", "-m", "merge")
	if err := os.WriteFile(filepath.Join(repo, "change"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "change")
	git("commit", "-m", "change")
	pieces, err := Build(context.Background(), t.TempDir(), []string{repo})
	if err != nil {
		t.Fatal(err)
	}
	foundIdea, foundBranch := false, false
	for _, p := range pieces {
		if p.Stage == Idea && p.Title == "An idea" {
			foundIdea = true
		}
		if p.Key == "branch:feature" && p.Stage == Merged {
			foundBranch = true
		}
	}
	if !foundIdea || !foundBranch {
		t.Fatalf("missing idea or merged branch: %+v", pieces)
	}
}

func TestBuildCombinesRecordsAndReportsInvalidRepositories(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(home, "none"))
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}
	git("init", "-b", "main")
	git("config", "user.email", "test@example.invalid")
	git("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "PLAN.md"), []byte("## Items\n- [ ] First\n- [x] Done\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "initial")
	git("checkout", "-b", "merged")
	git("commit", "--allow-empty", "-m", "merged work")
	git("checkout", "main")
	git("merge", "--no-ff", "merged", "-m", "merge")
	git("branch", "feature")
	git("branch", "untouched")
	worktree := filepath.Join(t.TempDir(), "worktree")
	git("worktree", "add", worktree, "feature")
	if err := os.WriteFile(filepath.Join(worktree, "new"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	git("-C", worktree, "add", "new")
	git("-C", worktree, "commit", "-m", "feature")
	if err := os.MkdirAll(filepath.Join(home, "runs", "r1"), 0700); err != nil {
		t.Fatal(err)
	}
	run := `{"id":"r1","task":{"prompt":"run","dir":` + strconv.Quote(worktree) + `}}`
	if err := os.WriteFile(filepath.Join(home, "runs", "r1", "task.json"), []byte(run), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "batches"), 0700); err != nil {
		t.Fatal(err)
	}
	batch := `{"id":"b1","started":"2030-01-01T00:00:00Z","dir":` + strconv.Quote(repo) + `,"tasks":[{"id":"t1","prompt":"task","branch":"feature","worktree":` + strconv.Quote(worktree) + `,"attempts":[{"run_id":"r1","gates":[{"command":"test","exit_code":1}]}]}]}`
	if err := os.WriteFile(filepath.Join(home, "batches", "b1.json"), []byte(batch), 0600); err != nil {
		t.Fatal(err)
	}
	invalid := t.TempDir()
	if err := os.WriteFile(filepath.Join(invalid, ".git"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	pieces, err := Build(context.Background(), home, []string{repo, invalid})
	if err == nil {
		t.Fatal("invalid repository was not reported")
	}
	if len(pieces) != 4 {
		t.Fatalf("pieces: %+v", pieces)
	}
	if pieces[0].Stage != Idea || pieces[1].Stage != Branch {
		t.Fatalf("order: %+v", pieces)
	}
	var feature *Piece
	for i := range pieces {
		if pieces[i].Key == "branch:feature" {
			feature = &pieces[i]
		}
		if i > 0 && rank(pieces[i-1].Stage) > rank(pieces[i].Stage) {
			t.Fatalf("order: %+v", pieces)
		}
	}
	if feature == nil || feature.Stage != Gates || feature.Stuck != "gate test failed" || len(feature.Links.Runs) != 1 || len(feature.Links.Batches) != 1 || canonical(feature.Links.Worktree) != canonical(worktree) {
		t.Fatalf("feature: %+v", feature)
	}
}
