package gc

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cristiangirlea/harness/internal/gitretry"
)

func TestWorktreeCommandsRetryIncompleteMetadata(t *testing.T) {
	for _, operation := range []string{"list", "remove"} {
		t.Run(operation, func(t *testing.T) {
			f := newFixture(t)
			wt := filepath.Join(t.TempDir(), "task")
			git(t, f.repo, "worktree", "add", "-b", "retry-task", wt)
			admin := filepath.Join(f.repo, ".git", "worktrees", "half")
			if err := os.MkdirAll(admin, 0755); err != nil {
				t.Fatal(err)
			}
			for name, body := range map[string]string{"gitdir": filepath.ToSlash(filepath.Join(f.repo, "half", ".git")) + "\n", "commondir": ""} {
				if err := os.WriteFile(filepath.Join(admin, name), []byte(body), 0644); err != nil {
					t.Fatal(err)
				}
			}
			old := gitretry.TestHookBeforeRetry
			t.Cleanup(func() { gitretry.TestHookBeforeRetry = old })
			retries := 0
			gitretry.TestHookBeforeRetry = func() {
				retries++
				if err := os.RemoveAll(admin); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			if operation == "list" {
				_, err = listWorktrees(context.Background(), f.repo)
			} else {
				err = removeGitWorktree(context.Background(), Worktree{Repo: f.repo, Path: wt})
			}
			if err != nil || retries != 1 {
				t.Fatalf("error = %v, retries = %d", err, retries)
			}
		})
	}
}
