package batch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/gitretry"
)

func TestAddWorktreePreservesIncompleteWorktreeError(t *testing.T) {
	root := initRepo(t)
	admin := filepath.Join(root, ".git", "worktrees", "half")
	if err := os.MkdirAll(admin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(admin, "commondir"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(admin, "gitdir"), []byte(filepath.ToSlash(filepath.Join(root, "half", ".git"))+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := addWorktree(context.Background(), root, TaskResult{
		Branch: "task", Worktree: filepath.Join(t.TempDir(), "task"),
	}, "HEAD")
	if err == nil || !strings.Contains(err.Error(), "commondir") || strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error = %v, want original commondir failure", err)
	}
}

func TestGitOutUsesEnglishLocale(t *testing.T) {
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	t.Setenv("LANGUAGE", "de")
	out, err := gitOut(context.Background(), initRepo(t), "-c", `alias.retry-env=!printf '%s/%s' "$LC_ALL" "$LANGUAGE"`, "retry-env")
	if err != nil || out != "C/" {
		t.Fatalf("output = %q, error = %v", out, err)
	}
}

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
			oldHook := gitretry.TestHookBeforeRetry
			t.Cleanup(func() { gitretry.TestHookBeforeRetry = oldHook })
			retries := 0
			gitretry.TestHookBeforeRetry = func() {
				retries++
				if err := os.RemoveAll(admin); err != nil {
					t.Fatal(err)
				}
			}
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
			if err != nil {
				t.Fatal(err)
			}
			// Some Git versions prune incomplete entries without a retry.
			if retries != 1 && family != "prune" {
				t.Fatalf("retries = %d, want 1", retries)
			}
		})
	}
}
