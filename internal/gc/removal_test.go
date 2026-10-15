package gc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Build once: these cases exercise removal, not Scan's proof of merge.
func TestGCRemovesOnlySafeSubmoduleWorktrees(t *testing.T) {
	f, wt := submoduleFixture(t)
	w := Worktree{Repo: f.repo, Path: wt, Branch: "done", Head: git(t, wt, "rev-parse", "HEAD"), Verdict: Remove}
	// Bound Git for Windows shell startup contention across independent copies.
	slots := make(chan struct{}, 4)
	copyFixture := func(t *testing.T) (*fixture, string, []Worktree) {
		t.Helper()
		slots <- struct{}{}
		t.Cleanup(func() { <-slots })
		root, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.CopyFS(root, os.DirFS(f.root)); err != nil {
			t.Fatal(err)
		}
		copy := &fixture{t: t, root: root, repo: filepath.Join(root, "repo")}
		path := filepath.Join(root, "wt")
		// Git stores absolute pointers in these two files. The submodule's
		// gitfile and core.worktree are relative and need no repair.
		copy.write(path, ".git", "gitdir: "+filepath.ToSlash(filepath.Join(copy.repo, ".git/worktrees/wt"))+"\n")
		copy.write(copy.repo, ".git/worktrees/wt/gitdir", filepath.ToSlash(filepath.Join(path, ".git"))+"\n")
		for _, name := range []string{".git/config", ".git/worktrees/wt/modules/module/config"} {
			data, err := os.ReadFile(filepath.Join(copy.repo, name))
			if err != nil {
				t.Fatal(err)
			}
			copy.write(copy.repo, name, strings.ReplaceAll(string(data), filepath.ToSlash(f.root), filepath.ToSlash(root)))
		}
		cw := w
		cw.Repo, cw.Path = copy.repo, path
		return copy, path, []Worktree{cw}
	}
	for _, tc := range []struct {
		name string
		run  func(*testing.T, func(*testing.T) (*fixture, string, []Worktree))
	}{
		{"clean", removesCleanSubmodule},
		{"files", leavesSubmoduleChanges},
		{"history", keepsSubmoduleHistory},
		{"nested", checksNestedSubmodules},
		{"orphaned", keepsOrphanedSubmodules},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.run(t, copyFixture)
		})
	}
	// These replace a package hook and must finish before parallel cases run.
	t.Run("unreadable", func(t *testing.T) { keepsUnreadableSubmodule(t, copyFixture) })
	t.Run("parent-changed", func(t *testing.T) { keepsParentChanges(t, copyFixture) })
}

func removesCleanSubmodule(t *testing.T, copyFixture func(*testing.T) (*fixture, string, []Worktree)) {
	f, wt, wts := copyFixture(t)
	removed, errs := Apply(context.Background(), wts)
	if len(removed) != 1 || len(errs) != 0 {
		t.Fatalf("removed %d, errors %v", len(removed), errs)
	}
	if exists(wt) || exists(filepath.Join(f.repo, ".git/refs/heads/done")) {
		t.Fatal("worktree or branch remains")
	}
	if exists(filepath.Join(f.repo, ".git/worktrees/wt")) {
		t.Fatal("worktree registration remains after prune")
	}
}

func leavesSubmoduleChanges(t *testing.T, copyFixture func(*testing.T) (*fixture, string, []Worktree)) {
	for _, name := range []string{"README.md", "notes.txt", "ignored.txt"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, wt, wts := copyFixture(t)
			f.write(filepath.Join(wt, "module"), name, "keep this\n")
			gone, err := removeWorktree(context.Background(), wts[0])
			if gone || err == nil {
				t.Fatalf("removed %v, error %v", gone, err)
			}
			if !exists(filepath.Join(wt, "module", name)) || !exists(filepath.Join(f.repo, ".git/refs/heads/done")) {
				t.Fatal("submodule file or branch lost")
			}
		})
	}
}

func submoduleFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, root: root, repo: filepath.Join(root, "repo")}
	git(t, root, "init", "-q", "-b", "main", f.repo)
	git(t, f.repo, "config", "user.email", "test@example.com")
	git(t, f.repo, "config", "user.name", "Test")
	f.write(f.repo, "README.md", "parent\n")
	git(t, f.repo, "add", ".")
	git(t, f.repo, "commit", "-q", "-m", "init")
	sub := filepath.Join(f.root, "sub")
	git(t, f.root, "init", "-q", "-b", "main", sub)
	git(t, sub, "config", "user.email", "test@example.com")
	git(t, sub, "config", "user.name", "Test")
	f.write(sub, "README.md", "submodule\n")
	f.write(sub, ".gitignore", "ignored.txt\n")
	git(t, sub, "add", ".")
	git(t, sub, "commit", "-q", "-m", "init")
	wt := filepath.Join(root, "wt")
	git(t, f.repo, "worktree", "add", "-q", "-b", "done", wt)
	git(t, wt, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "module")
	git(t, wt, "config", "submodule.module.ignore", "all")
	git(t, filepath.Join(wt, "module"), "config", "user.name", "Test")
	git(t, filepath.Join(wt, "module"), "config", "user.email", "test@example.com")
	git(t, wt, "commit", "-q", "-am", "add submodule")
	return f, wt
}

func keepsSubmoduleHistory(t *testing.T, copyFixture func(*testing.T) (*fixture, string, []Worktree)) {
	for _, kind := range []string{"commit-ignore-all", "stash", "local-branch", "deinitialized"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			f, wt, wts := copyFixture(t)
			module := filepath.Join(wt, "module")
			f.write(module, "README.md", "keep this work\n")
			ref := "HEAD"
			if kind == "stash" || kind == "deinitialized" {
				git(t, module, "stash", "push", "-q")
				ref = "refs/stash"
			} else {
				git(t, module, "checkout", "--detach", "-q")
				git(t, module, "commit", "-q", "-am", "local work")
				if kind == "local-branch" {
					git(t, module, "branch", "local-work")
					git(t, module, "checkout", "--detach", "-q", "HEAD^")
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
			gone, err := removeWorktree(context.Background(), wts[0])
			if gone || err == nil || !exists(wt) {
				t.Fatalf("removed %v, error %v; worktree must stay", gone, err)
			}
			if !exists(filepath.Join(f.repo, ".git/worktrees/wt")) || !exists(filepath.Join(f.repo, ".git/refs/heads/done")) {
				t.Fatal("worktree registration or parent branch lost")
			}
			gitdir := filepath.Join(f.repo, ".git/worktrees/wt/modules/module")
			if got := git(t, f.repo, "--git-dir="+gitdir, "rev-parse", ref); got != oid {
				t.Fatalf("submodule %s = %s, want %s", ref, got, oid)
			}
		})
	}
}

func checksNestedSubmodules(t *testing.T, copyFixture func(*testing.T) (*fixture, string, []Worktree)) {
	f, wt, wts := copyFixture(t)
	module := filepath.Join(wt, "module")
	git(t, module, "-c", "protocol.file.allow=always", "submodule", "add", "-q", filepath.Join(f.root, "sub"), "nested")
	git(t, module, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "-am", "add nested submodule")
	// The clean-removal case must not leave this commit only in the submodule.
	git(t, module, "push", "-q", "origin", "HEAD:refs/heads/nested-fixture")
	git(t, wt, "commit", "-q", "-am", "update submodule")
	wts[0].Head = git(t, wt, "rev-parse", "HEAD")
	git(t, module, "config", "submodule.nested.ignore", "all")
	f.write(filepath.Join(module, "nested"), "ignored.txt", "keep\n")
	gone, err := removeWorktree(context.Background(), wts[0])
	if gone || err == nil || !exists(filepath.Join(module, "nested", "ignored.txt")) {
		t.Fatalf("removed %v, error %v; nested file must stay", gone, err)
	}
	if err := os.Remove(filepath.Join(module, "nested", "ignored.txt")); err != nil {
		t.Fatal(err)
	}
	removed, errs := Apply(context.Background(), wts)
	if len(removed) != 1 || len(errs) != 0 || exists(wt) {
		t.Fatalf("removed %d, errors %v; clean nested submodule must go", len(removed), errs)
	}
}

func keepsOrphanedSubmodules(t *testing.T, copyFixture func(*testing.T) (*fixture, string, []Worktree)) {
	for _, nested := range []bool{false, true} {
		name := "direct"
		if nested {
			name = "nested"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, wt, wts := copyFixture(t)
			parent, path := wt, "module"
			if nested {
				parent, path = filepath.Join(wt, "module"), "nested"
				git(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", "-q", filepath.Join(f.root, "sub"), path)
				git(t, parent, "commit", "-q", "-am", "add nested submodule")
			}
			module := filepath.Join(parent, path)
			gitdir := git(t, module, "rev-parse", "--absolute-git-dir")
			f.write(module, "README.md", "stashed work to keep\n")
			git(t, module, "-c", "user.name=Test", "-c", "user.email=test@example.com", "stash", "push", "-q")
			oid := git(t, module, "rev-parse", "refs/stash")
			git(t, parent, "rm", "-q", path)
			git(t, parent, "commit", "-q", "-am", "remove submodule")
			if nested {
				// Keep the indexed parent's history on its remote so only the
				// unindexed nested repository prevents removal.
				git(t, parent, "push", "-q", "origin", "HEAD:refs/heads/removed-nested")
				git(t, wt, "commit", "-q", "-am", "update submodule")
			}
			wts[0].Head = git(t, wt, "rev-parse", "HEAD")
			removed, errs := Apply(context.Background(), wts)
			if len(removed) != 0 || len(errs) == 0 || !exists(wt) {
				t.Fatalf("removed %d, errors %v; orphaned repository must keep the worktree", len(removed), errs)
			}
			if !exists(filepath.Join(f.repo, ".git/worktrees/wt")) || !exists(filepath.Join(f.repo, ".git/refs/heads/done")) {
				t.Fatal("worktree registration or parent branch lost")
			}
			if got := git(t, f.repo, "--git-dir="+gitdir, "--work-tree="+wt, "rev-parse", "refs/stash"); got != oid {
				t.Fatalf("orphaned stash = %s, want %s", got, oid)
			}
		})
	}
}

func keepsUnreadableSubmodule(t *testing.T, copyFixture func(*testing.T) (*fixture, string, []Worktree)) {
	f, wt, wts := copyFixture(t)
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
	if len(removed) != 0 || len(errs) == 0 || !exists(wt) || !exists(filepath.Join(f.repo, ".git/refs/heads/done")) {
		t.Fatalf("removed %d, errors %v; worktree and branch must stay", len(removed), errs)
	}
}

func keepsParentChanges(t *testing.T, copyFixture func(*testing.T) (*fixture, string, []Worktree)) {
	f, wt, wts := copyFixture(t)
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
