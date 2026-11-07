//go:build windows

package tools

import (
	"os/exec"
	"testing"

	"github.com/cristiangirlea/harness/internal/nowindow"
)

func TestStartTreeKeepsTheHiddenWindowNextToItsOwnFlags(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "exit 0")
	nowindow.Set(cmd)
	tree, err := StartTree(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if !cmd.SysProcAttr.HideWindow || cmd.SysProcAttr.CreationFlags&createSuspended == 0 {
		t.Fatalf("got %+v, want HideWindow kept with the suspended start", cmd.SysProcAttr)
	}
}
