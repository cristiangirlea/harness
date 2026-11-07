//go:build windows

package nowindow

import (
	"os/exec"
	"syscall"
)

// Set hides the console a child allocates when hrn has none; an inherited console is unaffected.
func Set(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
}
