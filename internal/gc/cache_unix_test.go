//go:build unix

package gc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestGCDiskFreeNamesUnixMountPoint(t *testing.T) {
	root := tempRoot(t)
	d, err := diskFree(root)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(d.Name) {
		t.Fatalf("mount name %q is not absolute", d.Name)
	}
	info, err := os.Stat(d.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("mount name %q is not a directory", d.Name)
	}
	if rel, err := filepath.Rel(d.Name, root); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("mount %q does not contain %q: relative path %q, error %v", d.Name, root, rel, err)
	}
	sys := info.Sys().(*syscall.Stat_t)
	if want := fmt.Sprintf("device %d", sys.Dev); d.ID != want {
		t.Errorf("drive ID = %q, want %q", d.ID, want)
	}
	parent := filepath.Dir(d.Name)
	if parent != d.Name {
		parentInfo, err := os.Stat(parent)
		if err != nil {
			t.Fatal(err)
		}
		if parentInfo.Sys().(*syscall.Stat_t).Dev == sys.Dev {
			t.Errorf("mount %q and parent %q have the same device", d.Name, parent)
		}
	}
}

func TestGCApplyPutsBackCacheLockedJustAfterMoveOnUnix(t *testing.T) {
	root := tempRoot(t)
	cache := tagged(t, filepath.Join(root, "target"), 30*time.Hour, 10)
	cs := caches(t, []string{root}, nil, cacheOpt)
	previous := renameCache
	var release func()
	renameCache = func(src, dst string) error {
		if err := os.Rename(src, dst); err != nil {
			return err
		}
		release = holdLock(t, filepath.Join(dst, "debug", ".cargo-lock"))
		return nil
	}
	t.Cleanup(func() {
		renameCache = previous
		if release != nil {
			release()
		}
	})

	removed, errs := ApplyCaches(context.Background(), cs)
	if len(removed) != 0 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "kept, a build started") {
		t.Fatalf("removed %+v, errors %v", removed, errs)
	}
	if !isCacheDir(cache) || !exists(filepath.Join(cache, "debug", "deps", "libx.rlib")) {
		t.Errorf("cache was not returned whole after a build took its lock: %s", cache)
	}
}

// holdLock holds a profile's .cargo-lock the way Cargo does on Unix: an
// exclusive flock on an open descriptor, which another descriptor's
// non-blocking flock cannot take, in this process or any other.
func holdLock(t *testing.T, path string) (release func()) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		t.Fatal(err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
}
