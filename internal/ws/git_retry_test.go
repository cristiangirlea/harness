package ws

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSwitchRetriesWhileAnotherWorktreeIsBeingCreated(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	f := newFixture(t)
	git(t, f.repo, "branch", "existing")
	admin := f.halfAddedWorktree()
	done := make(chan error, 1)
	go func() {
		_, err := gitOut(context.Background(), f.repo, "switch", "existing")
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("switch returned before the other worktree finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := os.RemoveAll(admin); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := git(t, f.repo, "symbolic-ref", "--short", "HEAD"); strings.TrimSpace(got) != "existing" {
		t.Fatalf("branch = %q", got)
	}
}
