package batch

import (
	"errors"
	"os"
	"syscall"
)

const (
	errSharingViolation syscall.Errno = 32
	errLockViolation    syscall.Errno = 33
)

func renameBusy(err error) bool {
	return errors.Is(err, os.ErrPermission) || errors.Is(err, errSharingViolation) || errors.Is(err, errLockViolation)
}
