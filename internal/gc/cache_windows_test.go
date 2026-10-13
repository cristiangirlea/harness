//go:build windows

package gc

import (
	"os"
	"testing"
)

// holdLock holds a profile's .cargo-lock the way Cargo does on Windows: open
// for the build's duration, so an open that shares nothing fails.
func holdLock(t *testing.T, path string) (release func()) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	return func() { f.Close() }
}
