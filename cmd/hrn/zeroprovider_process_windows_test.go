//go:build windows

package main

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

type zeroProviderProcessEntry struct {
	Size, Usage, PID             uint32
	HeapID                       uintptr
	ModuleID, Threads, ParentPID uint32
	Priority                     int32
	Flags                        uint32
	Exe                          [260]uint16
}

func prepareZeroProviderCommand(cmd *exec.Cmd) { cmd.WaitDelay = 5 * time.Second }

func assertNoZeroProviderChildren(t *testing.T, pid int, started time.Time) {
	t.Helper()
	kernel := syscall.NewLazyDLL("kernel32.dll")
	snapshot := kernel.NewProc("CreateToolhelp32Snapshot")
	first := kernel.NewProc("Process32FirstW")
	next := kernel.NewProc("Process32NextW")
	open := kernel.NewProc("OpenProcess")
	times := kernel.NewProc("GetProcessTimes")
	h, _, err := snapshot.Call(2, 0)
	if h == ^uintptr(0) {
		t.Fatalf("process snapshot: %v", err)
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	entry := zeroProviderProcessEntry{Size: uint32(unsafe.Sizeof(zeroProviderProcessEntry{}))}
	ok, _, err := first.Call(h, uintptr(unsafe.Pointer(&entry)))
	if ok == 0 {
		t.Fatalf("process snapshot first: %v", err)
	}
	parents := map[uint32]bool{uint32(pid): true}
	entries := []zeroProviderProcessEntry{}
	for ok != 0 {
		entries = append(entries, entry)
		ok, _, err = next.Call(h, uintptr(unsafe.Pointer(&entry)))
	}
	if err != syscall.ERROR_NO_MORE_FILES {
		t.Fatalf("process snapshot next: %v", err)
	}
	for changed := true; changed; {
		changed = false
		for _, e := range entries {
			if parents[e.ParentPID] && !parents[e.PID] {
				hproc, _, openErr := open.Call(0x1000, 0, uintptr(e.PID))
				if hproc == 0 {
					t.Errorf("open process %d: %v", e.PID, openErr)
					continue
				}
				var created, exited, kernelTime, userTime syscall.Filetime
				ok, _, timeErr := times.Call(hproc, uintptr(unsafe.Pointer(&created)), uintptr(unsafe.Pointer(&exited)), uintptr(unsafe.Pointer(&kernelTime)), uintptr(unsafe.Pointer(&userTime)))
				syscall.CloseHandle(syscall.Handle(hproc))
				if ok == 0 {
					t.Errorf("process %d times: %v", e.PID, timeErr)
					continue
				}
				if time.Unix(0, created.Nanoseconds()).Before(started) {
					continue
				}
				parents[e.PID] = true
				changed = true
				t.Errorf("child process %d of %d remains", e.PID, e.ParentPID)
			}
		}
	}
}
