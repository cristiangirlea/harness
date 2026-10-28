//go:build !windows

package batch

import (
	"fmt"
	"os"
	"path/filepath"
)

func lockTempForCleanup(dir string) (func(), error) {
	sub := filepath.Join(dir, "held")
	if err := os.Mkdir(sub, 0700); err != nil {
		return nil, err
	}
	file := filepath.Join(sub, "file")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		return nil, err
	}
	if err := os.Chmod(sub, 0500); err != nil {
		return nil, err
	}
	if err := os.Remove(file); err == nil {
		os.Chmod(sub, 0700)
		return nil, fmt.Errorf("permissions do not prevent removal")
	}
	return func() { os.Chmod(sub, 0700) }, nil
}
