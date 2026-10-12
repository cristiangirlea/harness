//go:build unix

package gc

import (
	"os"
	"syscall"
	"testing"
)

// holdLock holds a profile's .cargo-lock the way Cargo does on Unix: an
// exclusive flock on an open descriptor, which another descriptor's
// non-blocking flock cannot take, in this process or any other.
func holdLock(t *testing.T, path string) (release func()) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		t.Fatal(err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
}
