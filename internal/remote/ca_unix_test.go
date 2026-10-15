//go:build unix

package remote

import (
	"syscall"
	"testing"
)

func makeCAFIFO(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
}
