//go:build unix

package tools

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func treeProcessState(pid int) (string, bool, error) {
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(err, os.ErrNotExist) {
		// Platforms without procfs have no portable creation-time syscall.
		if _, e := os.Stat("/proc"); errors.Is(e, os.ErrNotExist) {
			return strconv.Itoa(pid), true, nil
		}
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	fields := strings.Fields(string(data)[strings.LastIndex(string(data), ")")+1:])
	if len(fields) < 20 {
		return "", false, fmt.Errorf("short process stat for %d", pid)
	}
	return fields[19], fields[0] != "Z", nil
}
