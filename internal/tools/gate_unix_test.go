//go:build unix

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The gate keeps its command in a process group of its own, so a cancel must ask the gate to
// end that tree rather than kill the gate alone.
func TestCancellingAGatedCommandEndsItsWholeTree(t *testing.T) {
	ws, tl, _ := gatedWorkspace(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		raw, _ := json.Marshal(map[string]any{"command": "sleep 60 & echo $! > child.pid; wait", "timeout_sec": 120})
		_, err := tl.Run(ctx, ws, raw)
		done <- err
	}()
	pidFile := filepath.Join(ws.Root, "child.pid")
	var pid int
	for deadline := time.Now().Add(30 * time.Second); pid == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("the command did not start its child")
		}
		if data, err := os.ReadFile(pidFile); err == nil && strings.HasSuffix(string(data), "\n") {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("run_command did not return after the cancel")
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if processGone(pid) {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the command's child %d outlived the cancel", pid)
		}
	}
}

// A gate that ignores the terminate request is killed once WaitDelay passes.
func TestAGateThatIgnoresTerminateIsStillEnded(t *testing.T) {
	old := gateGrace
	gateGrace = time.Second
	t.Cleanup(func() { gateGrace = old })
	ws, tl, _ := gatedWorkspace(t)
	start := time.Now()
	out, err := call(t, ws, tl, map[string]any{"command": "fake-deaf sleep 12", "timeout_sec": 2})
	if err == nil || err.Error() != "command timed out" {
		t.Fatalf("run_command = %q, %v; want a timeout", out, err)
	}
	// Two seconds of timeout, one of grace and five of WaitDelay, well before the sleep ends.
	if took := time.Since(start); took > 11*time.Second {
		t.Fatalf("the deaf gate was ended after %s", took)
	}
}

// processGone counts a zombie as gone: a container's first process may never reap it.
func processGone(pid int) bool {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return true
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	i := strings.LastIndexByte(string(data), ')')
	return i >= 0 && len(data) > i+2 && data[i+2] == 'Z'
}
