//go:build unix

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
)

type signalBackend struct{}

func (signalBackend) Name() string            { return "stub-signal" }
func (signalBackend) Detect() (string, error) { return "test stub", nil }
func (signalBackend) Run(ctx context.Context, _ agent.Task, _ func(agent.Event)) agent.Result {
	if err := os.WriteFile(os.Getenv("HRN_SIGNAL_READY"), []byte("ready"), 0600); err != nil {
		panic(err)
	}
	<-ctx.Done()
	return agent.Result{Backend: "stub-signal", Status: agent.StatusCanceled, Error: ctx.Err().Error()}
}

func TestHelperRunWaitsForSignal(t *testing.T) {
	if os.Getenv("HRN_SIGNAL_HELPER") != "1" {
		return
	}
	agent.Register(signalBackend{})
	code, err := cmdRun([]string{"-b", "stub-signal", "-C", os.Getenv("HRN_SIGNAL_DIR"), "wait"})
	if err != nil || code != 1 {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestRunContextEndsOnTerminationSignals(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			dir := t.TempDir()
			ready := filepath.Join(dir, "ready")
			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperRunWaitsForSignal$")
			cmd.Env = append(os.Environ(), "HRN_SIGNAL_HELPER=1", "HRN_SIGNAL_DIR="+dir, "HRN_SIGNAL_READY="+ready, "HRN_HOME="+filepath.Join(dir, "records"))
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Process.Kill()
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				if !time.Now().Before(deadline) {
					t.Fatal("run backend never became ready")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("signal killed the helper instead of cancelling the run context: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("run context did not end on the signal")
			}
		})
	}
}
