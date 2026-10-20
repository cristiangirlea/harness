//go:build unix

package tools

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"syscall"
	"time"
)

// Tree tracks a process group; setsid children can escape it.
type Tree struct {
	pgid int
}

// StartTree starts a cancellable group; Wait and Close are required.
func StartTree(cmd *exec.Cmd) (*Tree, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pgid = 0
	if cmd.Cancel != nil {
		cmd.Cancel = func() error {
			if err := (&Tree{pgid: cmd.Process.Pid}).Kill(); err != nil {
				return cmd.Process.Kill()
			}
			return nil
		}
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &Tree{pgid: cmd.Process.Pid}, nil
}

// Contained reports group assignment.
func (t *Tree) Contained() bool { return true }

// StartWarnings is empty on Unix.
func (t *Tree) StartWarnings() []string { return nil }

// Close is a no-op on Unix.
func (t *Tree) Close() {}

// Kill kills even leaderless groups; gone succeeds.
func (t *Tree) Kill() error {
	if t.pgid <= 0 {
		return fmt.Errorf("process tree has no process group")
	}
	err := syscall.Kill(-t.pgid, syscall.SIGKILL)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return fmt.Errorf("kill process group %d: %w", t.pgid, err)
}

// WaitGone excludes Linux zombies; other Unix probes are lower bounds.
func (t *Tree) WaitGone(timeout time.Duration) error {
	var countLive func() (int, error)
	if runtime.GOOS == "linux" {
		countLive = func() (int, error) { return countRunningGroup("/proc", t.pgid) }
	}
	return waitGroupGone(t.pgid, timeout, func() error { return syscall.Kill(-t.pgid, 0) }, countLive)
}
