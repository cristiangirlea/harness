// Package gitretry handles Git's brief exposure of incomplete worktree metadata.
package gitretry

import (
	"context"
	"strings"
	"time"
)

// TestHookBeforeRetry, when non-nil, runs between attempts. Tests must set it
// only while no other Output calls are running and restore it afterward.
var TestHookBeforeRetry func()

// Output runs a Git command, retrying once after a short pause if Git could
// not read a worktree's commondir while another process was creating it.
// run must create a fresh command on each call.
func Output(ctx context.Context, run func() ([]byte, error)) ([]byte, error) {
	out, err := run()
	if err == nil {
		return out, nil
	}
	text := strings.ReplaceAll(string(out), `\`, "/")
	matching := false
	for _, line := range strings.Split(text, "\n") {
		if path, ok := strings.CutPrefix(line, "fatal: failed to read "); ok {
			path, tail, found := strings.Cut(path, "/commondir")
			_, name, worktree := strings.Cut(path, ".git/worktrees/")
			matching = found && worktree && name != "" && !strings.Contains(name, "/") &&
				(tail == "" || strings.HasPrefix(tail, ":"))
			if matching {
				break
			}
		}
	}
	if !matching {
		return out, err
	}
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return out, ctx.Err()
	case <-timer.C:
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if TestHookBeforeRetry != nil {
			TestHookBeforeRetry()
		}
		return run()
	}
}
