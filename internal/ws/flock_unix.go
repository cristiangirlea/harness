//go:build unix

package ws

import (
	"errors"
	"os"
	"syscall"
)

// tryLock takes an exclusive lock on f without waiting, and reports false
// when another handle holds it. The lock goes with the open file, so the
// kernel drops it when its holder closes f or dies.
func tryLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
