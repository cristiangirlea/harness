package gc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func slotCache(t *testing.T, idle time.Duration) string {
	t.Helper()
	root := tempRoot(t)
	cache := tagged(t, filepath.Join(root, ".hrn", "slots", "1", "target"), idle, 30)
	for _, name := range []string{".fingerprint", "build"} {
		dir := filepath.Join(cache, "debug", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dir, "artifact")
		if err := os.WriteFile(file, []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
		touch(t, time.Now().Add(-idle), file, dir)
	}
	for _, profile := range []string{"debug", filepath.Join("triple", "release")} {
		dir := filepath.Join(cache, profile, "incremental")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{filepath.Join(dir, "object"), filepath.Join(cache, profile, ".cargo-lock")} {
			if err := os.WriteFile(name, []byte("1234567890"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		at := time.Now().Add(-idle)
		touch(t, at, filepath.Join(dir, "object"), dir, filepath.Join(cache, profile, ".cargo-lock"), filepath.Dir(dir))
	}
	touch(t, time.Now().Add(-idle), filepath.Join(cache, "triple"), cache)
	return cache
}

func TestGCTrimsIdleSlotOverCapAndKeepsOtherArtifacts(t *testing.T) {
	cache := slotCache(t, 3*time.Hour)
	opt := cacheOpt
	opt.SlotCap = 1
	cs := caches(t, []string{cache}, nil, opt)
	c := findCache(t, cs, cache)
	if c.Verdict != Trim || c.TrimSize != 20 {
		t.Fatalf("got %+v", c)
	}
	done, errs := ApplyCaches(context.Background(), cs)
	if len(done) != 1 || len(errs) != 0 {
		t.Fatalf("done %v, errors %v", done, errs)
	}
	for _, p := range []string{"CACHEDIR.TAG", "debug/.cargo-lock", "debug/deps/libx.rlib", "debug/.fingerprint/artifact", "debug/build/artifact", "triple/release/.cargo-lock"} {
		if !exists(filepath.Join(cache, p)) {
			t.Errorf("lost %s", p)
		}
	}
	for _, p := range []string{"debug/incremental", "triple/release/incremental"} {
		if exists(filepath.Join(cache, p)) {
			t.Errorf("kept %s", p)
		}
	}
}

func TestGCSlotCapRespectsMinimumIdleAndIdleRule(t *testing.T) {
	for _, tc := range []struct {
		idle time.Duration
		cap  int64
		want Verdict
	}{
		{time.Hour, 1, Keep}, {3 * time.Hour, 0, Keep}, {3 * time.Hour, 10000, Keep}, {30 * time.Hour, 10000, Trim},
	} {
		cache := slotCache(t, tc.idle)
		opt := cacheOpt
		opt.SlotCap = tc.cap
		if got := findCache(t, caches(t, []string{cache}, nil, opt), cache).Verdict; got != tc.want {
			t.Errorf("idle %s cap %d: %s", tc.idle, tc.cap, got)
		}
	}
}

func TestGCRechecksSlotLockBeforeTrimming(t *testing.T) {
	cache := slotCache(t, 30*time.Hour)
	cs := caches(t, []string{cache}, nil, cacheOpt)
	release := holdLock(t, filepath.Join(cache, "debug", ".cargo-lock"))
	defer release()
	done, errs := ApplyCaches(context.Background(), cs)
	if len(done) != 0 || len(errs) == 0 || !exists(filepath.Join(cache, "debug", "incremental")) {
		t.Fatalf("done %v, errors %v", done, errs)
	}
}

func TestGCKeepsSlotLockedDuringScan(t *testing.T) {
	cache := slotCache(t, 30*time.Hour)
	release := holdLock(t, filepath.Join(cache, "debug", ".cargo-lock"))
	defer release()
	if c := findCache(t, caches(t, []string{cache}, nil, cacheOpt), cache); c.Verdict != Keep {
		t.Fatalf("got %+v", c)
	}
}

func TestGCFreeBelowTrimsSlotWithoutRemovingIt(t *testing.T) {
	cache := slotCache(t, 3*time.Hour)
	opt := cacheOpt
	opt.FreeBelow = 1000
	opt.Drive = func(string) (Drive, error) { return Drive{ID: "disk", Free: 1}, nil }
	cs := caches(t, []string{cache}, nil, opt)
	if c := findCache(t, cs, cache); c.Verdict != Trim {
		t.Fatalf("got %+v", c)
	}
	done, errs := ApplyCaches(context.Background(), cs)
	if len(done) != 1 || len(errs) != 0 || !exists(filepath.Join(cache, "CACHEDIR.TAG")) {
		t.Fatalf("done %v, errors %v", done, errs)
	}
}
