//go:build windows

package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"github.com/cristiangirlea/harness/internal/tools"
)

var treeTestKernel = syscall.NewLazyDLL("kernel32.dll")
var treeTestTimes = treeTestKernel.NewProc("GetProcessTimes")
var treeTestExit = treeTestKernel.NewProc("GetExitCodeProcess")

func treeProcessState(pid int) (string, bool, error) {
	h, err := syscall.OpenProcess(0x1000, false, uint32(pid))
	if err == syscall.Errno(87) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer syscall.CloseHandle(h)
	var created, exited, kernel, user syscall.Filetime
	if ok, _, err := treeTestTimes.Call(uintptr(h), uintptr(unsafe.Pointer(&created)), uintptr(unsafe.Pointer(&exited)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user))); ok == 0 {
		return "", false, err
	}
	var code uint32
	if ok, _, err := treeTestExit.Call(uintptr(h), uintptr(unsafe.Pointer(&code))); ok == 0 {
		return "", false, err
	}
	return fmt.Sprintf("%d:%d", created.HighDateTime, created.LowDateTime), code == 259, nil
}

func TestCLIStopReportsUnavailableContainmentOnce(t *testing.T) {
	err := stopCLITree(&tools.Tree{}, context.Canceled)
	if err == nil || strings.Count(err.Error(), "not contained") != 1 || strings.Contains(err.Error(), "kill process tree") || !errors.Is(err, context.Canceled) {
		t.Fatalf("want one missing-containment explanation preserving cancellation: %v", err)
	}
}
