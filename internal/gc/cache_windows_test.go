//go:build windows

package gc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// holdLock holds a profile's .cargo-lock the way Cargo does on Windows: open
// for the build's duration, so an open that shares nothing fails.
func holdLock(t *testing.T, path string) (release func()) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	return func() { f.Close() }
}
