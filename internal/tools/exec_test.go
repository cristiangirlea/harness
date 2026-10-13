package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestBoundedBufferKeepsHeadAndTail(t *testing.T) {
	b := &boundedBuffer{keepHead: 10, keepTail: 5}
	for i := 0; i < 10; i++ {
		_, _ = b.Write([]byte("0123456789"))
	}
	got := b.String()
	if !strings.HasPrefix(got, "0123456789\n") || !strings.HasSuffix(got, "\n56789") || !strings.Contains(got, "[... 85 bytes truncated ...]") {
		t.Errorf("String() = %q", got)
	}
	small := &boundedBuffer{keepHead: 10, keepTail: 5}
	_, _ = small.Write([]byte("hello "))
	_, _ = small.Write([]byte("world"))
	if got := small.String(); got != "hello world" {
		t.Errorf("short output must be verbatim: %q", got)
	}
}

func TestRunShellExitCodeAndOutput(t *testing.T) {
	res, err := RunShell(context.Background(), t.TempDir(), "echo hi && exit 3", 0, nil)
	if err != nil || res.ExitCode != 3 || res.TimedOut || !strings.Contains(res.Output, "hi") {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestRunShellTimeoutKillsTheTree(t *testing.T) {
	// The timeout leaves a busy Windows PC time to start Git's bash and print
	// "started" (500ms did not, #68).
	const timeout = 3 * time.Second
	started := time.Now()
	// sleep is a grandchild of the shell. Killing only the shell would leave it
	// holding the output pipe until RunShell's WaitDelay (5s) gives up, so such
	// a run returns at timeout+5s at the earliest; the bound below is tighter.
	res, err := RunShell(context.Background(), t.TempDir(), "echo started; sleep 30; echo late", timeout, nil)
	if err != nil || !res.TimedOut || res.ExitCode != -1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if !strings.Contains(res.Output, "started") || strings.Contains(res.Output, "late") {
		t.Errorf("output = %q", res.Output)
	}
	if elapsed := time.Since(started); elapsed > timeout+4*time.Second {
		t.Errorf("took %s; the process tree was not killed", elapsed)
	}
}
