//go:build !windows

package nowindow

import (
	"os/exec"
	"testing"
)

func TestSetLeavesTheCommandAloneOffWindows(t *testing.T) {
	cmd := exec.Command("git", "--version")
	Set(cmd)
	if cmd.SysProcAttr != nil {
		t.Fatalf("got %+v, want no attributes", cmd.SysProcAttr)
	}
}
