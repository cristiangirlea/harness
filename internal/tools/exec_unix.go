//go:build unix

package tools

import (
	"errors"
	"fmt"
	"os/exec"
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
			if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
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

// WaitGone waits until the process group no longer exists. Unix's kill probe
// does not expose a member count, so a timeout reports the lower bound of one.
func (t *Tree) WaitGone(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Kill(-t.pgid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("query process group %d: %w", t.pgid, err)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("process group %d: at least 1 process still alive after %s", t.pgid, timeout)
		}
		time.Sleep(min(10*time.Millisecond, time.Until(deadline)))
	}
}
