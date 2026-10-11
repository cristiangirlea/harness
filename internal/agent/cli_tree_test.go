package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

type treeIdentity struct {
	PID  int
	Born string
}

// TestHelperProcessTree starts its child before doing any other work.
func TestHelperProcessTree(t *testing.T) {
	switch os.Getenv("HRN_TREE_HELPER") {
	case "parent":
		child := exec.Command(os.Args[0], "-test.run=^TestHelperProcessTree$")
		child.Env = append(os.Environ(), "HRN_TREE_HELPER=sleep")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			panic(err)
		}
		born, _, err := treeProcessState(child.Process.Pid)
		if err != nil {
			panic(err)
		}
		data, _ := json.Marshal(treeIdentity{child.Process.Pid, born})
		if err := os.WriteFile(os.Getenv("HRN_TREE_PID_FILE"), data, 0600); err != nil {
			panic(err)
		}
		time.Sleep(time.Minute)
		os.Exit(0)
	case "sleep":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

func readTreeIdentity(t *testing.T, file string) treeIdentity {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(file)
		var id treeIdentity
		if err == nil && json.Unmarshal(data, &id) == nil && id.PID > 0 {
			t.Cleanup(func() {
				born, alive, err := treeProcessState(id.PID)
				if err == nil && alive && born == id.Born {
					p, _ := os.FindProcess(id.PID)
					if p != nil {
						_ = p.Kill()
					}
				}
			})
			return id
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("helper did not write its grandchild identity")
	return treeIdentity{}
}

func assertTreeProcessGone(t *testing.T, id treeIdentity) {
	t.Helper()
	born, alive, err := treeProcessState(id.PID)
	if err != nil {
		t.Fatal(err)
	}
	if alive && born == id.Born {
		t.Fatalf("grandchild %d (created %s) still running", id.PID, id.Born)
	}
}

func TestCLICancellationAndTimeoutKillTheGrandchild(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout=%v", timeout), func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "child.json")
			t.Setenv("HRN_TREE_HELPER", "parent")
			t.Setenv("HRN_TREE_PID_FILE", file)
			backend := &CLIBackend{
				name: os.Args[0], promptStdin: true,
				args: func(Task, string) []string { return []string{"-test.run=^TestHelperProcessTree$"} },
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			task := Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Mode: ModeAsk}}
			if timeout {
				task.Timeout = Duration(2 * time.Second)
			}
			done := make(chan Result, 1)
			go func() { done <- backend.Run(ctx, task, func(Event) {}) }()
			id := readTreeIdentity(t, file)
			if !timeout {
				cancel()
			}
			select {
			case res := <-done:
				want := StatusCanceled
				wantError := context.Canceled.Error()
				if timeout {
					want = StatusTimeout
					wantError = context.DeadlineExceeded.Error()
				}
				if res.Status != want {
					t.Fatalf("status = %s, want %s: %s", res.Status, want, res.Error)
				}
				if res.Error != wantError {
					t.Fatalf("tree cleanup was not verified: %s", res.Error)
				}
				assertTreeProcessGone(t, id)
			case <-time.After(8 * time.Second):
				t.Fatal("CLI did not return within the bounded cancellation wait")
			}
		})
	}
}

func TestCLIAlreadyCanceledReturnsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := helperCLI(nil).Run(ctx, Task{Dir: t.TempDir()}, func(Event) {})
	if res.Status != StatusCanceled {
		t.Fatalf("status=%s error=%s", res.Status, res.Error)
	}
}
