//go:build windows

package ws

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

const (
	lockfileFailImmediately = 0x1
	lockfileExclusiveLock   = 0x2
	errorLockViolation      = 33
)

// Nonblocking byte locks release on close or death, preventing stale ownership.
func tryLock(f *os.File) (bool, error) {
	var ol syscall.Overlapped
	ok, _, err := procLockFileEx.Call(f.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	if ok != 0 {
		return true, nil
	}
	if err == syscall.Errno(errorLockViolation) {
		return false, nil
	}
	return false, err
}

func unlockFile(f *os.File) error {
	var ol syscall.Overlapped
	if ok, _, err := procUnlockFileEx.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&ol))); ok == 0 {
		return err
	}
	return nil
}
