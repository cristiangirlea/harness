//go:build unix

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func treeProcessState(pid int) (string, bool, error) {
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(err, os.ErrNotExist) {
		// Platforms without procfs have no portable creation-time syscall.
		if _, e := os.Stat("/proc"); errors.Is(e, os.ErrNotExist) {
			return strconv.Itoa(pid), true, nil
		}
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	fields := strings.Fields(string(data)[strings.LastIndex(string(data), ")")+1:])
	if len(fields) < 20 {
		return "", false, fmt.Errorf("short process stat for %d", pid)
	}
	return fields[19], fields[0] != "Z", nil
}

func TestHelperCLIEscapedChild(t *testing.T) {
	if os.Getenv("HRN_ESCAPE_HELPER") != "1" {
		return
	}
	child := exec.Command(os.Args[0], "-test.run=^TestHelperProcessTree$")
	child.Env = append(os.Environ(), "HRN_TREE_HELPER=sleep")
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
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
}

func TestCLICancellationBoundsReadsWhenAnEscapedChildKeepsStdoutOpen(t *testing.T) {
	file := filepath.Join(t.TempDir(), "child.json")
	t.Setenv("HRN_ESCAPE_HELPER", "1")
	t.Setenv("HRN_TREE_PID_FILE", file)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backend := &CLIBackend{
		name: os.Args[0], promptStdin: true,
		args: func(Task, string) []string { return []string{"-test.run=^TestHelperCLIEscapedChild$"} },
	}
	done := make(chan Result, 1)
	go func() { done <- backend.Run(ctx, Task{Dir: t.TempDir()}, func(Event) {}) }()
	id := readTreeIdentity(t, file)
	cancel()
	select {
	case res := <-done:
		if res.Status != StatusCanceled || res.Error != context.Canceled.Error() {
			t.Fatalf("want canceled after verifying the original group is gone: %+v", res)
		}
		born, alive, err := treeProcessState(id.PID)
		if err != nil || !alive || born != id.Born {
			t.Fatalf("the setsid child must still hold the pipes open: alive=%v err=%v", alive, err)
		}
	case <-time.After(17 * time.Second):
		t.Fatal("cancellation exceeded the roughly 15-second read/wait bounds")
	}
}
