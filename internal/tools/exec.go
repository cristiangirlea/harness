package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/cristiangirlea/harness/internal/gitenv"
	"github.com/cristiangirlea/harness/internal/retrieval"
)

// ShellResult describes a started command.
type ShellResult struct {
	Output    string // combined stdout and stderr; head and tail are kept when long
	OutputCut bool   // full capture exceeded its ceiling; Output is the existing cut
	ExitCode  int    // -1 when the command was killed by timeout or cancellation
	TimedOut  bool
}

// RunShell kills the whole tree on timeout or cancellation; only a start failure or ErrWait is an error.
// A non-zero exit is a result, a zero timeout means none, and a nil env is hrn's own.
func RunShell(ctx context.Context, dir, command string, timeout time.Duration, env []string) (ShellResult, error) {
	return runShell(ctx, dir, command, timeout, env, false)
}

const maxCapture = 8 << 20

// ErrWait marks a shell that started but whose Wait failed, so callers do not
// report it as a start failure.
var ErrWait = errors.New("waiting for the shell failed")

var waitShell = (*exec.Cmd).Wait

func runShell(ctx context.Context, dir, command string, timeout time.Duration, env []string, capture bool) (ShellResult, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	_, argv := Shell()
	parts := argv(command)
	cmd := exec.CommandContext(ctx, parts[0], parts[1:]...)
	cmd.Dir = dir
	// Strip explicit operator credentials too.
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = retrieval.ExecutorEnv(gitenv.WithoutRepoVars(env))
	out := &boundedBuffer{keepHead: maxOutput * 2 / 3, keepTail: maxOutput / 3}
	if capture {
		out.fullLimit = maxCapture
	}
	cmd.Stdout, cmd.Stderr = out, out
	// Descendants may hold pipes open.
	cmd.WaitDelay = 5 * time.Second
	tree, err := StartTree(cmd)
	if err != nil {
		return ShellResult{}, err
	}
	defer tree.Close()

	err = waitShell(cmd)
	res := ShellResult{Output: out.String(), OutputCut: out.fullDropped, TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded)}
	var exitErr *exec.ExitError
	switch {
	case err == nil, errors.Is(err, exec.ErrWaitDelay):
		// ErrWaitDelay means open pipes, not a failed command.
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	case ctx.Err() != nil:
		// Killed before it produced an exit status.
	default:
		return res, fmt.Errorf("%w: %v", ErrWait, err)
	}
	if ctx.Err() != nil {
		res.ExitCode = -1
	}
	return res, nil
}

// Cap bytes without racing stdout/stderr copies.
type boundedBuffer struct {
	mu                 sync.Mutex
	keepHead, keepTail int
	head, tail         []byte
	dropped            int
	// Retain head/tail on capture overflow.
	fullLimit   int
	full        []byte
	fullDropped bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if b.fullLimit > 0 && !b.fullDropped {
		if n <= b.fullLimit-len(b.full) {
			b.full = append(b.full, p...)
		} else {
			b.full, b.fullDropped = nil, true
		}
	}
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
	if b.fullLimit > 0 && !b.fullDropped {
		return string(b.full)
	}
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
