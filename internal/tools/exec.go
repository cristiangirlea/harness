package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

// ShellResult is the outcome of a command RunShell managed to start.
type ShellResult struct {
	Output   string // combined stdout and stderr; head and tail are kept when long
	ExitCode int    // -1 when the command was killed by timeout or cancellation
	TimedOut bool
}

// RunShell runs one command line through Shell() in dir. A zero timeout means
// none. On timeout or cancellation the whole process tree is killed, not just
// the shell, so a hung test cannot keep running and hold the directory. Only
// a failure to start is an error; a non-zero exit is a result.
func RunShell(ctx context.Context, dir, command string, timeout time.Duration) (ShellResult, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	_, argv := Shell()
	parts := argv(command)
	cmd := exec.CommandContext(ctx, parts[0], parts[1:]...)
	cmd.Dir = dir
	out := &boundedBuffer{keepHead: maxOutput * 2 / 3, keepTail: maxOutput / 3}
	cmd.Stdout, cmd.Stderr = out, out
	// Stop waiting on the pipes if something survived the kill or was left
	// running in the background.
	cmd.WaitDelay = 5 * time.Second
	tree, err := StartTree(cmd)
	if err != nil {
		return ShellResult{}, err
	}
	defer tree.Close()

	err = cmd.Wait()
	res := ShellResult{Output: out.String(), TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded)}
	var exitErr *exec.ExitError
	switch {
	case err == nil, errors.Is(err, exec.ErrWaitDelay):
		// Exited with status 0; ErrWaitDelay only means a grandchild kept the pipes open.
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	case ctx.Err() != nil:
		// Killed before it produced an exit status.
	default:
		return res, err
	}
	if ctx.Err() != nil {
		res.ExitCode = -1
	}
	return res, nil
}

// boundedBuffer keeps the first keepHead and last keepTail bytes written to
// it, so a chatty command cannot grow memory without bound while its output
// still renders the way Truncate would. Safe for the concurrent stdout and
// stderr copies exec runs.
type boundedBuffer struct {
	mu                 sync.Mutex
	keepHead, keepTail int
	head, tail         []byte
	dropped            int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if room := b.keepHead - len(b.head); room > 0 {
		take := min(room, len(p))
		b.head = append(b.head, p[:take]...)
		p = p[take:]
	}
	if len(p) > 0 {
		b.tail = append(b.tail, p...)
		if len(b.tail) > 2*b.keepTail {
			b.dropped += len(b.tail) - b.keepTail
			b.tail = append(b.tail[:0:0], b.tail[len(b.tail)-b.keepTail:]...)
		}
	}
	return n, nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	tail, dropped := b.tail, b.dropped
	if len(tail) > b.keepTail {
		dropped += len(tail) - b.keepTail
		tail = tail[len(tail)-b.keepTail:]
	}
	if dropped == 0 {
		return string(b.head) + string(tail)
	}
	return fmt.Sprintf("%s\n\n[... %d bytes truncated ...]\n\n%s", b.head, dropped, tail)
}
