package gitretry

import (
	"context"
	"errors"
	"testing"
)

func TestOutputRetriesOnlyTheIncompleteWorktreeErrorOnce(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, message := range []string{
		"fatal: failed to read .git/worktrees/4/commondir: No such file or directory",
		"fatal: failed to read E:/repo/.git/worktrees/4/commondir",
		`fatal: failed to read E:\repo\.git\worktrees\4\commondir`,
		"fatal: failed to read .git/commondir",
		"fatal: failed to read .git/worktrees/4/HEAD",
		"fatal: failed to read .git/worktrees/4/commondirectory",
		"fatal: failed to read .git/worktrees/4/sub/commondir",
		"fatal: branch is checked out elsewhere",
	} {
		t.Run(message, func(t *testing.T) {
			for _, recovers := range []bool{false, true} {
				calls := 0
				failure := errors.New("exit status 128")
				out, err := Output(context.Background(), func() ([]byte, error) {
					calls++
					if recovers && calls == 2 {
						return []byte("done"), nil
					}
					return []byte(message), failure
				})
				want := 1
				if message == "fatal: failed to read .git/worktrees/4/commondir: No such file or directory" ||
					message == "fatal: failed to read E:/repo/.git/worktrees/4/commondir" ||
					message == `fatal: failed to read E:\repo\.git\worktrees\4\commondir` {
					want = 2
				}
				if calls != want {
					t.Fatalf("calls = %d, want %d", calls, want)
				}
				if recovers && want == 2 {
					if err != nil || string(out) != "done" {
						t.Fatalf("output = %q, error = %v", out, err)
					}
				} else if err != failure || string(out) != message {
					t.Fatalf("output = %q, error = %v", out, err)
				}
			}
		})
	}
}

func TestOutputStopsWaitingWhenCanceled(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	_, err := Output(ctx, func() ([]byte, error) {
		calls++
		cancel()
		return []byte("fatal: failed to read .git/worktrees/4/commondir"), errors.New("exit status 128")
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("calls = %d, error = %v", calls, err)
	}
}
