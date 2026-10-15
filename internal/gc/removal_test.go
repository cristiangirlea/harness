package gc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGCRemovesAWorktreeContainingACleanSubmodule(t *testing.T) {
	f, wt := submoduleFixture(t)
	wts := f.scan()
	wantVerdict(t, find(t, wts, wt), Remove, "merged")
	removed, errs := Apply(context.Background(), wts)
	if len(removed) != 1 || len(errs) != 0 {
		t.Fatalf("removed %d, errors %v", len(removed), errs)
	}
	if exists(wt) || git(t, f.repo, "branch", "--list", "done") != "" {
		t.Fatal("worktree or branch remains")
	}
	entries, err := listWorktrees(context.Background(), f.repo)
	if err != nil || len(entries) != 1 {
		t.Fatalf("worktrees after prune: %v, %v", entries, err)
	}
}

func TestGCLeavesSubmoduleChangesHiddenFromTheParent(t *testing.T) {
	for _, name := range []string{"README.md", "notes.txt", "ignored.txt"} {
		t.Run(name, func(t *testing.T) {
			f, wt := submoduleFixture(t)
			wts := f.scan()
			git(t, wt, "config", "submodule.module.ignore", "all")
			f.write(filepath.Join(wt, "module"), name, "keep this\n")
			removed, errs := Apply(context.Background(), wts)
			if len(removed) != 0 || len(errs) == 0 {
				t.Fatalf("removed %d, errors %v", len(removed), errs)
			}
			if !exists(filepath.Join(wt, "module", name)) || git(t, f.repo, "branch", "--list", "done") == "" {
				t.Fatal("submodule file or branch lost")
			}
		})
	}
}

func submoduleFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newFixture(t)
	sub := filepath.Join(f.root, "sub")
	git(t, f.root, "init", "-q", "-b", "main", sub)
	git(t, sub, "config", "user.email", "test@example.com")
	git(t, sub, "config", "user.name", "Test")
	f.write(sub, "README.md", "submodule\n")
	f.write(sub, ".gitignore", "ignored.txt\n")
	git(t, sub, "add", ".")
	git(t, sub, "commit", "-q", "-m", "init")
	wt, _ := f.worktree("wt", "done")
	git(t, wt, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "module")
	git(t, wt, "commit", "-q", "-am", "add submodule")
	head := git(t, wt, "rev-parse", "HEAD")
	f.pr(1, "done", "MERGED", head, 2*time.Hour)
	return f, wt
}

func TestGCKeepsSubmoduleHistory(t *testing.T) {
	for _, kind := range []string{"commit-ignore-all", "stash", "local-branch", "deinitialized"} {
		t.Run(kind, func(t *testing.T) {
			f, wt := submoduleFixture(t)
			module := filepath.Join(wt, "module")
			git(t, module, "config", "user.name", "Test")
			git(t, module, "config", "user.email", "test@example.com")
			base := git(t, module, "rev-parse", "HEAD")
			git(t, module, "checkout", "--detach", "-q")
			wts := f.scan()
			wantVerdict(t, find(t, wts, wt), Remove, "merged")
			git(t, wt, "config", "submodule.module.ignore", "all")
			f.write(module, "README.md", "keep this work\n")
			ref := "HEAD"
			if kind == "stash" || kind == "deinitialized" {
				git(t, module, "stash", "push", "-q")
				ref = "refs/stash"
			} else {
				git(t, module, "commit", "-q", "-am", "local work")
				if kind == "local-branch" {
					git(t, module, "branch", "local-work")
					git(t, module, "checkout", "--detach", "-q", base)
					ref = "refs/heads/local-work"
				}
			}
			oid := git(t, module, "rev-parse", ref)
			if kind == "deinitialized" {
				git(t, wt, "submodule", "deinit", "-f", "--", "module")
			}
			if out := git(t, wt, "status", "--porcelain"); out != "" {
				t.Fatalf("parent must hide submodule work: %s", out)
			}
			removed, errs := Apply(context.Background(), wts)
			if len(removed) != 0 || len(errs) == 0 || !exists(wt) {
				t.Fatalf("removed %d, errors %v; worktree must stay", len(removed), errs)
			}
			entries, err := listWorktrees(context.Background(), f.repo)
			if err != nil || !listed(wt, entryPaths(entries)) || git(t, f.repo, "branch", "--list", "done") == "" {
				t.Fatalf("worktree registration or parent branch lost: %v", err)
			}
			if kind == "deinitialized" {
				git(t, wt, "-c", "protocol.file.allow=always", "submodule", "update", "--init")
			}
			if got := git(t, module, "rev-parse", ref); got != oid {
				t.Fatalf("submodule %s = %s, want %s", ref, got, oid)
			}
		})
	}
}

func TestGCChecksNestedSubmodulesBeforeRemovingAWorktree(t *testing.T) {
	f, wt := submoduleFixture(t)
	module := filepath.Join(wt, "module")
	git(t, module, "-c", "protocol.file.allow=always", "submodule", "add", "-q", filepath.Join(f.root, "sub"), "nested")
	git(t, module, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "-am", "add nested submodule")
	// The clean-removal case must not leave this commit only in the submodule.
	git(t, module, "push", "-q", "origin", "HEAD:refs/heads/nested-fixture")
	git(t, wt, "commit", "-q", "-am", "update submodule")
	f.prs = nil
	f.pr(1, "done", "MERGED", git(t, wt, "rev-parse", "HEAD"), 2*time.Hour)
	wts := f.scan()
	wantVerdict(t, find(t, wts, wt), Remove, "merged")
	git(t, wt, "config", "submodule.module.ignore", "all")
	git(t, module, "config", "submodule.nested.ignore", "all")
	f.write(filepath.Join(module, "nested"), "ignored.txt", "keep\n")
	removed, errs := Apply(context.Background(), wts)
	if len(removed) != 0 || len(errs) == 0 || !exists(filepath.Join(module, "nested", "ignored.txt")) {
		t.Fatalf("removed %d, errors %v; nested file must stay", len(removed), errs)
	}
	if err := os.Remove(filepath.Join(module, "nested", "ignored.txt")); err != nil {
		t.Fatal(err)
	}
	removed, errs = Apply(context.Background(), wts)
	if len(removed) != 1 || len(errs) != 0 || exists(wt) {
		t.Fatalf("removed %d, errors %v; clean nested submodule must go", len(removed), errs)
	}
}

func TestGCKeepsTheWorktreeWhenItsSubmoduleCannotBeChecked(t *testing.T) {
	f, wt := submoduleFixture(t)
	wts := f.scan()
	original := removeGitWorktree
	t.Cleanup(func() { removeGitWorktree = original })
	removeGitWorktree = func(ctx context.Context, w Worktree) error {
		err := original(ctx, w)
		// Simulate a submodule becoming unreadable between the parent check
		// and the fallback, without needing platform-specific permissions.
		f.write(filepath.Join(w.Path, "module"), ".git", "gitdir: missing\n")
		return err
	}
	removed, errs := Apply(context.Background(), wts)
	if len(removed) != 0 || len(errs) == 0 || !exists(wt) || git(t, f.repo, "branch", "--list", "done") == "" {
		t.Fatalf("removed %d, errors %v; worktree and branch must stay", len(removed), errs)
	}
}

func TestGCKeepsParentChangesMadeDuringSubmoduleRemoval(t *testing.T) {
	f, wt := submoduleFixture(t)
	wts := f.scan()
	original := removeGitWorktree
	t.Cleanup(func() { removeGitWorktree = original })
	removeGitWorktree = func(ctx context.Context, w Worktree) error {
		err := original(ctx, w)
		// Another process can write after Apply's first recheck.
		f.write(w.Path, "README.md", "keep this parent edit\n")
		return err
	}
	removed, errs := Apply(context.Background(), wts)
	if len(removed) != 0 || len(errs) == 0 || !exists(wt) {
		t.Fatalf("removed %d, errors %v; changed parent must stay", len(removed), errs)
	}
	if got, err := os.ReadFile(filepath.Join(wt, "README.md")); err != nil || string(got) != "keep this parent edit\n" {
		t.Fatalf("parent edit lost: %q, %v", got, err)
	}
}

func TestGCRecoversWhenGitDeregistersAWorktreeBeforeReportingFailure(t *testing.T) {
	for _, leftover := range []string{"empty", "missing", "file", "registered"} {
		t.Run(leftover, func(t *testing.T) {
			f := newFixture(t)
			wt, head := f.worktree("wt", "done")
			f.pr(1, "done", "MERGED", head, 2*time.Hour)
			wts := f.scan()
			original := removeGitWorktree
			t.Cleanup(func() { removeGitWorktree = original })
			removeGitWorktree = func(ctx context.Context, w Worktree) error {
				if leftover != "registered" {
					if err := original(ctx, w); err != nil {
						t.Fatal(err)
					}
					if leftover != "missing" {
						if err := os.Mkdir(w.Path, 0o755); err != nil {
							t.Fatal(err)
						}
					}
					if leftover == "file" {
						f.write(w.Path, "notes.txt", "keep\n")
					}
				}
				return errors.New("directory removal failed")
			}
			removed, errs := Apply(context.Background(), wts)
			if leftover == "empty" || leftover == "missing" {
				if len(removed) != 1 || len(errs) != 0 || exists(wt) || git(t, f.repo, "branch", "--list", "done") != "" {
					t.Fatalf("removed %d, errors %v; worktree or branch remains", len(removed), errs)
				}
			} else if len(removed) != 0 || len(errs) == 0 || !exists(wt) || git(t, f.repo, "branch", "--list", "done") == "" {
				t.Fatalf("removed %d, errors %v; expected worktree and branch kept", len(removed), errs)
			}
		})
	}
}
