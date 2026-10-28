//go:build !windows

package gc

import "os"

func openLockForRead(path string) (*os.File, error) { return os.Open(path) }
