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
	res, err := RunShell(context.Background(), t.TempDir(), "echo hi && exit 3", 0)
	if err != nil || res.ExitCode != 3 || res.TimedOut || !strings.Contains(res.Output, "hi") {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestRunShellTimeoutKillsTheTree(t *testing.T) {
	started := time.Now()
	// sleep is a grandchild of the shell; without a tree kill it would keep
	// the output pipe open and Run would block for the full 30s.
	res, err := RunShell(context.Background(), t.TempDir(), "echo started; sleep 30; echo late", 500*time.Millisecond)
	if err != nil || !res.TimedOut || res.ExitCode != -1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if !strings.Contains(res.Output, "started") || strings.Contains(res.Output, "late") {
		t.Errorf("output = %q", res.Output)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("took %s; the process tree was not killed", elapsed)
	}
}
