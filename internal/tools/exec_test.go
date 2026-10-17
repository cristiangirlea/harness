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

func TestRunShellNilEnvironmentHidesWorkerAndScalerTokens(t *testing.T) {
	t.Setenv("HRN_WORKER_TOKEN", "worker-fixture")
	t.Setenv("HRN_SCALER_TOKEN", "scaler-fixture")
	command := "if [ -n \"$HRN_WORKER_TOKEN$HRN_SCALER_TOKEN\" ]; then echo leaked; else echo clean; fi"
	if name, _ := Shell(); name == "powershell" {
		command = "if ($env:HRN_WORKER_TOKEN -or $env:HRN_SCALER_TOKEN) { echo leaked } else { echo clean }"
	}
	result, err := RunShell(context.Background(), t.TempDir(), command, 0, nil)
	if err != nil || result.ExitCode != 0 || !strings.Contains(result.Output, "clean") || strings.Contains(result.Output, "leaked") {
		t.Fatalf("shell environment: result=%+v err=%v", result, err)
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

func TestFullCaptureKeepsExactlyTheCeilingAndDropsOneByteMore(t *testing.T) {
	for _, extra := range []int{0, 1} {
		b := &boundedBuffer{keepHead: 10, keepTail: 5, fullLimit: maxCapture}
		text := strings.Repeat("x", maxCapture-1) + "\n" + strings.Repeat("y", extra)
		for chunk := text; chunk != ""; {
			n := min(len(chunk), 32<<10)
			_, _ = b.Write([]byte(chunk[:n]))
			chunk = chunk[n:]
		}
		got := b.String()
		if dropped := extra > 0; b.fullDropped != dropped || (got == text) == dropped {
			t.Fatalf("%d bytes: dropped=%v, whole=%v", len(text), b.fullDropped, got == text)
		}
	}
}
