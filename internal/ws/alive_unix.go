//go:build unix

package ws

import (
	"errors"
	"syscall"
)

// Signal 0 checks existence harmlessly; EPERM also proves existence.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
