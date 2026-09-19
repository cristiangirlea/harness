//go:build unix

package tools

import (
	"os/exec"
	"syscall"
)

type processTree struct{}

// newProcessTree puts the shell in its own process group so cancellation can
// kill the group, taking any command the shell started down with it.
func newProcessTree(cmd *exec.Cmd) processTree {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	return processTree{}
}

func (processTree) Attach(*exec.Cmd) {}
func (processTree) Close()           {}
