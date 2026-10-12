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
)

// errSharingViolation is ERROR_SHARING_VIOLATION: the file is open elsewhere
// in a way this open does not share.
const errSharingViolation syscall.Errno = 32

// diskFree asks the volume holding path how much it has free for this user,
// and names the volume by its drive or share.
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
	return Drive{ID: strings.ToUpper(filepath.VolumeName(samePath(abs))), Free: int64(avail)}, nil
}

// lockHeld reports whether a build holds the lock file. Cargo keeps
// .cargo-lock open while it builds, so an open that shares nothing fails with
// a sharing violation while one runs. A missing file is held by nobody.
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
