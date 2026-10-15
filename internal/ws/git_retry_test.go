package ws

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/gitretry"
)

func TestGitOutUsesEnglishLocale(t *testing.T) {
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	t.Setenv("LANGUAGE", "de")
	f := newFixture(t)
	out, err := gitOut(context.Background(), f.repo, "-c", `alias.retry-env=!printf '%s/%s' "$LC_ALL" "$LANGUAGE"`, "retry-env")
	if err != nil || out != "C/" {
		t.Fatalf("output = %q, error = %v", out, err)
	}
}

func TestSwitchRetriesWhileAnotherWorktreeIsBeingCreated(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	f := newFixture(t)
	git(t, f.repo, "branch", "existing")
	admin := f.halfAddedWorktree()
	oldHook := gitretry.TestHookBeforeRetry
	t.Cleanup(func() { gitretry.TestHookBeforeRetry = oldHook })
	retries := 0
	gitretry.TestHookBeforeRetry = func() {
		retries++
		if err := os.RemoveAll(admin); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := gitOut(context.Background(), f.repo, "switch", "existing"); err != nil {
		t.Fatal(err)
	}
	if retries != 1 {
		t.Fatalf("retries = %d, want 1", retries)
	}
	if got := git(t, f.repo, "symbolic-ref", "--short", "HEAD"); strings.TrimSpace(got) != "existing" {
		t.Fatalf("branch = %q", got)
	}
}
