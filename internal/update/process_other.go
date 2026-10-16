//go:build !windows && !linux && !darwin

package update

import (
	"fmt"
	"runtime"
	"syscall"
)

func systemProcesses() ([]Process, error) {
	return nil, fmt.Errorf("hrn update cannot list processes on %s; restart hrn watch by hand", runtime.GOOS)
}

func processLogs(int) (string, string) { return "", "" }

func detach(attr *syscall.SysProcAttr) { attr.Setsid = true }
