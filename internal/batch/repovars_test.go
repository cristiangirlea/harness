package batch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestABatchIgnoresARepositoryNamedInHrnsEnvironment(t *testing.T) {
	dir := initRepo(t)
	other := initRepo(t)
	head := git(t, other, "rev-parse", "HEAD")
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(other, ".git", "index"))
	res, err := Run(context.Background(), Spec{Dir: dir, Backend: "wb-ok", Worktrees: WorktreesFresh, Gate: []string{"test -f out.txt"},
		Tasks: []Task{{ID: "own", Prompt: "write out"}}}, nil)
	var checkoutErr error
	out := t.TempDir()
	if err == nil && res.Tasks[0].Commit != "" {
		checkoutErr = CheckoutTree(context.Background(), dir, res.Tasks[0].Commit, out)
	}
	for _, v := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"} {
		t.Setenv(v, "")
		os.Unsetenv(v)
	}
	defer Remove(res)
	if err != nil || len(res.Tasks) == 0 {
		t.Fatalf("batch = %v", err)
	}
	if res.Tasks[0].Status != StatusPass || res.Tasks[0].Commit == "" {
		t.Fatalf("batch = %v, task %+v; want a pass with its commit in the repository it was given", err, res.Tasks[0])
	}
	if checkoutErr != nil {
		t.Errorf("CheckoutTree: %v", checkoutErr)
	} else if _, err := os.Stat(filepath.Join(out, "out.txt")); err != nil {
		t.Errorf("CheckoutTree wrote no out.txt: %v", err)
	}
	if got := git(t, other, "rev-parse", "HEAD"); got != head {
		t.Errorf("the repository GIT_DIR named moved to %s", got)
	}
	if got := git(t, other, "status", "--porcelain"); got != "" {
		t.Errorf("the repository GIT_DIR named changed:\n%s", got)
	}
	if got := git(t, other, "branch", "--list", "hrn/*"); got != "" {
		t.Errorf("the repository GIT_DIR named got branches: %s", got)
	}
}
