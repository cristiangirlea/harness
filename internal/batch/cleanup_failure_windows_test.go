package batch

import (
	"os"
	"path/filepath"
	"syscall"
)

func lockTempForCleanup(dir string) (func(), error) {
	path := filepath.Join(dir, "held")
	if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
		return nil, err
	}
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return func() { syscall.CloseHandle(h) }, nil
}
