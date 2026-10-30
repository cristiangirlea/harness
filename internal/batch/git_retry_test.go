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
	if err == nil {
		// Whether worktree add reads the other worktrees' metadata depends
		// on git's version and code path; some (git 2.55 on Linux, at times)
		// add the worktree regardless, and then there is no error to keep.
		t.Skip("this git adds a worktree beside a half-written one, so there is no error to preserve")
	}
	if !strings.Contains(err.Error(), "commondir") || strings.Contains(err.Error(), "already exists") {
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

func TestAddWorktreeCleansBranchAfterFailure(t *testing.T) {
	root := initRepo(t)
	hooks := filepath.Join(root, ".git", "hooks")
	hook := filepath.Join(hooks, "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf leftover > untracked\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	tr := TaskResult{Branch: "failed-task", Worktree: filepath.Join(t.TempDir(), "task")}
	if err := addWorktree(context.Background(), root, tr, "HEAD"); err == nil {
		t.Fatal("expected add failure")
	}
	if got := git(t, root, "branch", "--list", tr.Branch); got != "" {
		t.Fatalf("branch left behind: %s", got)
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	if err := addWorktree(context.Background(), root, tr, "HEAD"); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestAddWorktreeKeepsExistingBranchOnFailure(t *testing.T) {
	root := initRepo(t)
	git(t, root, "branch", "existing-task")
	tr := TaskResult{Branch: "existing-task", Worktree: filepath.Join(t.TempDir(), "task")}
	if err := addWorktree(context.Background(), root, tr, "HEAD"); err == nil {
		t.Fatal("expected add failure")
	}
	if got := git(t, root, "branch", "--list", tr.Branch); got == "" {
		t.Fatal("existing branch was deleted")
	}
}

func TestAddWorktreeKeepsMovedBranchOnFailure(t *testing.T) {
	root := initRepo(t)
	base := git(t, root, "rev-parse", "HEAD")
	git(t, root, "commit", "--allow-empty", "-m", "other")
	moved := git(t, root, "rev-parse", "HEAD")
	hook := filepath.Join(root, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ngit update-ref refs/heads/failed-task "+moved+"\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	tr := TaskResult{Branch: "failed-task", Worktree: filepath.Join(t.TempDir(), "task")}
	if err := addWorktree(context.Background(), root, tr, base); err == nil {
		t.Fatal("expected add failure")
	}
	if got := git(t, root, "rev-parse", "refs/heads/failed-task"); got != moved {
		t.Fatalf("branch = %s, want %s", got, moved)
	}
}

func TestAddWorktreeKeepsExistingEmptyDirectoryOnFailure(t *testing.T) {
	root := initRepo(t)
	hook := filepath.Join(root, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "task")
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	tr := TaskResult{Branch: "failed-task", Worktree: path}
	if err := addWorktree(context.Background(), root, tr, "HEAD"); err == nil {
		t.Fatal("expected add failure")
	}
	if got := git(t, root, "branch", "--list", tr.Branch); got == "" {
		t.Fatal("branch was deleted")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("directory not kept: %v", err)
	}
}

func TestAddWorktreeCleansBranchForExistingNonemptyDirectory(t *testing.T) {
	root := initRepo(t)
	path := filepath.Join(t.TempDir(), "task")
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(path, "existing")
	if err := os.WriteFile(file, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	tr := TaskResult{Branch: "failed-task", Worktree: path}
	if err := addWorktree(context.Background(), root, tr, "HEAD"); err == nil {
		t.Fatal("expected add failure")
	}
	if got := git(t, root, "branch", "--list", tr.Branch); got != "" {
		t.Fatalf("branch left behind: %s", got)
	}
	if got, err := os.ReadFile(file); err != nil || string(got) != "keep" {
		t.Fatalf("existing file = %q, error = %v", got, err)
	}
}
