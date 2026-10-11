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

// Tree is a command's process group. A child that calls setsid can leave the
// group; we do not try to chase it.
type Tree struct {
	pgid int
}

// StartTree starts cmd in its own process group and makes its context's
// cancellation kill that group. The caller must Wait on cmd and Close the tree.
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

// Contained reports whether the command was placed in a process group.
func (t *Tree) Contained() bool { return true }

// Close has no group handle to release on Unix.
func (t *Tree) Close() {}

// Kill terminates the process group, including children left by an exited
// command. It succeeds if the group is already gone.
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

// WaitGone waits until no group member is running. Linux scans procfs to
// exclude unreaped zombies; elsewhere the group probe supplies a lower bound.
func (t *Tree) WaitGone(timeout time.Duration) error {
	var countLive func() (int, error)
	if runtime.GOOS == "linux" {
		countLive = func() (int, error) { return countRunningGroup("/proc", t.pgid) }
	}
	return waitGroupGone(t.pgid, timeout, func() error { return syscall.Kill(-t.pgid, 0) }, countLive)
}
