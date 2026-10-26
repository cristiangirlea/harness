//go:build darwin

package update

import (
	"fmt"
	"os/exec"
	"syscall"
)

// macOS has no standard-library process enumeration, so use ps.
func systemProcesses() ([]Process, error) {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,lstart=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	return parsePS(out, exec.LookPath), nil
}

// processLogs cannot see another process's open files here.
func processLogs(int) (string, string) { return "", "" }

func detach(attr *syscall.SysProcAttr) { attr.Setsid = true }
