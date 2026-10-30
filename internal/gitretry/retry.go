// Package gitretry tolerates transient worktree metadata.
package gitretry

import (
	"context"
	"strings"
	"time"
)

// TestHookBeforeRetry runs between attempts; set and restore it only while Output is idle.
var TestHookBeforeRetry func()

// Output retries commondir races; run must create fresh commands.
func Output(ctx context.Context, run func() ([]byte, error)) ([]byte, error) {
	out, err := run()
	if err == nil {
		return out, nil
	}
	text := strings.ReplaceAll(string(out), `\`, "/")
	matching := false
	for _, line := range strings.Split(text, "\n") {
		if path, ok := strings.CutPrefix(line, "fatal: failed to read "); ok {
			end := strings.LastIndex(path, "/commondir")
			if end < 0 {
				continue
			}
			tail := path[end+len("/commondir"):]
			path = path[:end]
			start := strings.LastIndex(path, ".git/worktrees/")
			if start < 0 {
				continue
			}
			name := path[start+len(".git/worktrees/"):]
			matching = name != "" && !strings.Contains(name, "/") &&
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
