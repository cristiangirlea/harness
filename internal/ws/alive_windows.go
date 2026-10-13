//go:build windows

package ws

import (
	"strconv"
	"syscall"
	"unsafe"
)

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procOpenProcess        = kernel32.NewProc("OpenProcess")
	procGetExitCodeProcess = kernel32.NewProc("GetExitCodeProcess")
	procGetProcessTimes    = kernel32.NewProc("GetProcessTimes")
)

const (
	processQueryLimitedInformation = 0x1000
	stillActive                    = 259
	errorAccessDenied              = 5
)

// alive reports whether a process with this pid is running. OpenProcess
// fails for a pid nobody holds; access denied means it runs under another
// user. A process that exited but whose handle someone still holds opens,
// and its exit code says it is gone.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, _, err := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if h == 0 {
		return err == syscall.Errno(errorAccessDenied)
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	var code uint32
	if ok, _, _ := procGetExitCodeProcess.Call(h, uintptr(unsafe.Pointer(&code))); ok == 0 {
		return true
	}
	return code == stillActive
}

// startID returns the process creation FILETIME in 100 ns ticks.
func startID(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	h, _, _ := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if h == 0 {
		return "", false
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	var created, exited, kernel, user syscall.Filetime
	ok, _, _ := procGetProcessTimes.Call(h,
		uintptr(unsafe.Pointer(&created)),
		uintptr(unsafe.Pointer(&exited)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)))
	if ok == 0 {
		return "", false
	}
	ticks := uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)
	return "win:" + strconv.FormatUint(ticks, 10), true
}
