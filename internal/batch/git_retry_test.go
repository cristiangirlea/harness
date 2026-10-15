package batch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGitCommandsRetryWhileAnotherWorktreeIsBeingCreated(t *testing.T) {
	for _, family := range []string{"list", "remove", "prune", "branch", "slot branch"} {
		t.Run(family, func(t *testing.T) {
			root := initRepo(t)
			wt := filepath.Join(t.TempDir(), "task")
			git(t, root, "worktree", "add", "-q", "-b", "task", wt)
			git(t, root, "branch", "unused")
			admin := filepath.Join(root, ".git", "worktrees", "half")
			if err := os.MkdirAll(admin, 0o755); err != nil {
				t.Fatal(err)
			}
			for name, body := range map[string]string{
				"gitdir":    filepath.ToSlash(filepath.Join(root, "half", ".git")) + "\n",
				"commondir": "",
			} {
				if err := os.WriteFile(filepath.Join(admin, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() {
				var err error
				switch family {
				case "list":
					var found bool
					found, err = isWorktree(root, wt)
					if err == nil && !found {
						err = os.ErrNotExist
					}
				case "remove":
					_, err = gitOut(context.Background(), root, "worktree", "remove", "--force", wt)
				case "prune":
					_, err = gitOut(context.Background(), root, "worktree", "prune")
				case "branch":
					_, err = gitOut(context.Background(), root, "branch", "-D", "unused")
				case "slot branch":
					err = removeSlotBranch(root, TaskResult{Branch: "unused"})
				}
				done <- err
			}()
			select {
			case err := <-done:
				if family == "prune" && err == nil {
					// Git versions that ignore an incomplete entry need no retry.
					return
				}
				t.Fatalf("%s returned before the other worktree finished: %v", family, err)
			case <-time.After(100 * time.Millisecond):
			}
			if err := os.RemoveAll(admin); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
