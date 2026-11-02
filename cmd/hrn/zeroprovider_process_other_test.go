//go:build !windows

package main

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func prepareZeroProviderCommand(cmd *exec.Cmd) {
	cmd.WaitDelay = 5 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func assertNoZeroProviderChildren(t *testing.T, pid int, _ time.Time) {
	t.Helper()
	if err := syscall.Kill(-pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("process group %d remains or cannot be checked: %v", pid, err)
	}
}
