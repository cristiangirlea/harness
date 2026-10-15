//go:build windows

package gc

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestGCDeletesTheBranchWhenOnlyAnOpenEmptyDirectoryRemains(t *testing.T) {
	f := newFixture(t)
	wt, head := f.worktree("wt", "done")
	f.pr(1, "done", "MERGED", head, 2*time.Hour)
	wts := f.scan()
	original := removeGitWorktree
	t.Cleanup(func() { removeGitWorktree = original })
	removeGitWorktree = func(ctx context.Context, w Worktree) error {
		if err := original(ctx, w); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(w.Path, 0o755); err != nil {
			t.Fatal(err)
		}
		path, err := syscall.UTF16PtrFromString(w.Path)
		if err != nil {
			t.Fatal(err)
		}
		h, err := syscall.CreateFile(path, syscall.GENERIC_READ, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
			nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { syscall.CloseHandle(h) })
		return errors.New("directory held open")
	}
	removed, errs := Apply(context.Background(), wts)
	if len(removed) != 1 || len(errs) != 1 || !exists(wt) {
		t.Fatalf("removed %d, errors %v; expected only the empty directory to remain", len(removed), errs)
	}
	if git(t, f.repo, "branch", "--list", "done") != "" {
		t.Fatal("branch remains although git removed the worktree")
	}
}
