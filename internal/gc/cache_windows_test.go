//go:build windows

package gc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestGCApplyKeepsAWholeCacheWithAnOpenFileOnWindows(t *testing.T) {
	root := tempRoot(t)
	cache := tagged(t, filepath.Join(root, "target"), 30*time.Hour, 100)
	cs := caches(t, []string{root}, nil, cacheOpt)
	wantCache(t, findCache(t, cs, cache), Remove, "idle 30h")

	f, err := os.Open(filepath.Join(cache, "debug", "deps", "libx.rlib"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	removed, errs := ApplyCaches(context.Background(), cs)
	if len(removed) != 0 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "kept, it could not be moved aside") {
		t.Fatalf("removed %+v, errors %v; want one move error", removed, errs)
	}
	for _, path := range []string{"CACHEDIR.TAG", filepath.Join("debug", ".cargo-lock"), filepath.Join("debug", "deps", "libx.rlib")} {
		if !exists(filepath.Join(cache, path)) {
			t.Errorf("cache lost %s", path)
		}
	}
	leftovers, err := filepath.Glob(filepath.Join(root, ".gc-*-target"))
	if err != nil || len(leftovers) != 0 {
		t.Errorf("rename left folders behind: %q, %v", leftovers, err)
	}
}

func TestGCTrimLockAllowsCargoOpenButBlocksItsLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".cargo-lock")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	unlock, err := holdCargoLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("cargo-style open: %v", err)
	}
	defer syscall.CloseHandle(h)
	var overlapped syscall.Overlapped
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
	r, _, _ := proc.Call(uintptr(h), 3, 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if r != 0 {
		t.Fatal("another exclusive lock succeeded")
	}
}

func holdLock(t *testing.T, path string) (release func()) {
	t.Helper()
	release, err := holdCargoLock(path)
	if err != nil {
		t.Fatal(err)
	}
	return release
}
