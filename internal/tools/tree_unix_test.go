//go:build unix

package tools

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
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

func TestWaitGoneIgnoresAnUnreapedLinuxZombie(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux procfs")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcessTree$")
	cmd.Env = append(os.Environ(), "HRN_TREE_HELPER=sleep")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Keep Wait deferred: the dead child must remain a zombie for the probe.
	defer cmd.Wait()
	defer cmd.Process.Kill()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", cmd.Process.Pid))
		if err != nil {
			t.Fatal(err)
		}
		pgid, state, err := parseProcStat(string(data))
		if err != nil {
			t.Fatal(err)
		}
		if state == 'Z' && pgid == cmd.Process.Pid {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatal("child did not become a zombie")
		}
		time.Sleep(time.Millisecond)
	}
	if err := syscall.Kill(-cmd.Process.Pid, 0); err != nil {
		t.Fatalf("the zombie must still make the group probe succeed: %v", err)
	}
	tree := &Tree{pgid: cmd.Process.Pid}
	if err := tree.WaitGone(30 * time.Millisecond); err != nil {
		t.Fatalf("an unreaped zombie is no longer running: %v", err)
	}
}
