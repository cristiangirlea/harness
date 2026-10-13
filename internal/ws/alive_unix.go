//go:build unix

package ws

import (
	"errors"
	"syscall"
)

// alive reports whether a process with this pid exists. Signal 0 checks
// without sending anything; EPERM means it exists under another user.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
