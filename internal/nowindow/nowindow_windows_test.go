//go:build windows

package nowindow

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestSetHidesTheWindowOfACommandWithoutAttributes(t *testing.T) {
	cmd := exec.Command("git", "--version")
	Set(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow {
		t.Fatalf("got %+v, want HideWindow set", cmd.SysProcAttr)
	}
}

func TestSetKeepsTheCreationFlagsAlreadyOnTheCommand(t *testing.T) {
	const createSuspended = 0x4
	cmd := exec.Command("git", "--version")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createSuspended}
	Set(cmd)
	if !cmd.SysProcAttr.HideWindow || cmd.SysProcAttr.CreationFlags != createSuspended {
		t.Fatalf("got %+v, want HideWindow with the flags kept", cmd.SysProcAttr)
	}
}
