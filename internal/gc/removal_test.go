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

func TestGCChecksNestedSubmodulesBeforeRemovingAWorktree(t *testing.T) {
	f, wt := submoduleFixture(t)
	module := filepath.Join(wt, "module")
	git(t, module, "-c", "protocol.file.allow=always", "submodule", "add", "-q", filepath.Join(f.root, "sub"), "nested")
	git(t, module, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "-am", "add nested submodule")
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
