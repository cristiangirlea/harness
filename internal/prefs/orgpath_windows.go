//go:build windows

package prefs

import (
	"fmt"
	"path/filepath"
	"syscall"
	"unsafe"
)

var procSystemWindowsDirectory = syscall.NewLazyDLL("kernel32.dll").NewProc("GetSystemWindowsDirectoryW")

func systemOrgPath() (string, error) {
	folder := make([]uint16, syscall.MAX_LONG_PATH)
	n, _, _ := procSystemWindowsDirectory.Call(uintptr(unsafe.Pointer(&folder[0])), uintptr(len(folder)))
	if n > 0 && n < uintptr(len(folder)) {
		dir := syscall.UTF16ToString(folder[:n])
		if filepath.IsAbs(dir) {
			root := filepath.VolumeName(dir) + string(filepath.Separator)
			return filepath.Join(root, "ProgramData", "hrn", "backends.json"), nil
		}
	}
	return "", fmt.Errorf("%w: its folder could not be determined", ErrOrganisation)
}
