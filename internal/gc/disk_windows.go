//go:build windows

package gc

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procGetDiskFreeSpaceExW = kernel32.NewProc("GetDiskFreeSpaceExW")
	procGetVolumePathNameW  = kernel32.NewProc("GetVolumePathNameW")
)

// An incompatible open handle yields ERROR_SHARING_VIOLATION.
const errSharingViolation syscall.Errno = 32

// Use the volume mount point: a drive letter can contain multiple mounted volumes.
func diskFree(path string) (Drive, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Drive{}, err
	}
	p, err := syscall.UTF16PtrFromString(abs)
	if err != nil {
		return Drive{}, err
	}
	var avail, total, free uint64
	r, _, e := procGetDiskFreeSpaceExW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&avail)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&free)))
	if r == 0 {
		return Drive{}, &os.PathError{Op: "GetDiskFreeSpaceEx", Path: abs, Err: e}
	}
	mount := make([]uint16, syscall.MAX_LONG_PATH)
	r, _, e = procGetVolumePathNameW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&mount[0])), uintptr(len(mount)))
	if r == 0 {
		return Drive{}, &os.PathError{Op: "GetVolumePathName", Path: abs, Err: e}
	}
	return Drive{ID: strings.ToUpper(syscall.UTF16ToString(mount)), Free: int64(avail)}, nil
}

// Cargo keeps the lock open while building; a no-sharing open detects its handle.
func lockHeld(path string) (bool, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	switch {
	case err == nil:
		syscall.CloseHandle(h)
		return false, nil
	case err == errSharingViolation:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	}
	return false, &os.PathError{Op: "open", Path: path, Err: err}
}

func holdCargoLock(path string) (func(), error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return func() { syscall.CloseHandle(h) }, nil
}
