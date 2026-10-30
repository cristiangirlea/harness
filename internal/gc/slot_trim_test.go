package gc

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	opt.SlotCap = 102
	file := filepath.Join(cache, "debug", "incremental", "object")
	if err := os.WriteFile(file, make([]byte, 60), 0o644); err != nil {
		t.Fatal(err)
	}
	touch(t, time.Now().Add(-3*time.Hour), file)
	cs := caches(t, []string{cache}, nil, opt)
	c := findCache(t, cs, cache)
	if c.Verdict != Trim || c.TrimSize != 70 {
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
	for _, p := range []string{"debug/deps/libx.rlib", "debug/.fingerprint/artifact", "debug/build/artifact"} {
		if !exists(filepath.Join(cache, p)) {
			t.Errorf("lost %s", p)
		}
	}
	if exists(filepath.Join(cache, "debug/incremental")) {
		t.Fatal("incremental remains")
	}
}

func TestGCSlotPathsUseWorkspaceNaming(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"x/slots/1/target", false}, {".hrn/slots/0/target", false}, {".hrn/slots/1/target", true}, {".hrn/slots/2/a/target", true}, {".hrn/slots/abc/target", false}, {".hrn/slots/01/target", false}, {".HRN/SLOTS/1/target", runtime.GOOS == "windows"},
	} {
		if got := slotCachePath(filepath.FromSlash(tc.path)); got != tc.want {
			t.Errorf("%s: %v", tc.path, got)
		}
	}
}

func TestGCRemovesMovedAndNonCargoCachesInsideSlots(t *testing.T) {
	for _, moved := range []bool{false, true} {
		root := tempRoot(t)
		name := ".pytest_cache"
		if moved {
			name = ".gc-42-target"
		}
		path := filepath.Join(root, ".hrn", "slots", "1", name)
		if moved {
			tagged(t, path, 30*time.Hour, 10)
		} else {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
			tag := filepath.Join(path, "CACHEDIR.TAG")
			if err := os.WriteFile(tag, []byte(cacheTagSignature), 0o644); err != nil {
				t.Fatal(err)
			}
			touch(t, time.Now().Add(-30*time.Hour), tag, path)
		}
		cs := caches(t, []string{path}, nil, cacheOpt)
		if c := findCache(t, cs, path); c.Verdict != Remove {
			t.Fatalf("%s: %+v", path, c)
		}
		_, errs := ApplyCaches(context.Background(), cs)
		if len(errs) != 0 || exists(path) {
			t.Fatalf("%s: %v", path, errs)
		}
	}
}

func TestGCSlotCapRemovesDepsHeavyCache(t *testing.T) {
	cache := slotCache(t, 3*time.Hour)
	opt := cacheOpt
	opt.SlotCap = 50
	cs := caches(t, []string{cache}, nil, opt)
	if c := findCache(t, cs, cache); c.Verdict != Remove {
		t.Fatalf("got %+v", c)
	}
	done, errs := ApplyCaches(context.Background(), cs)
	if len(done) != 1 || len(errs) != 0 || exists(cache) {
		t.Fatalf("done %v, errors %v, cache exists %v", done, errs, exists(cache))
	}
}

func TestGCSlotCeilingSafetyAndTrimProjection(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		idle                     time.Duration
		cap                      int64
		locked, largeIncremental bool
		want                     Verdict
	}{
		{"locked", 3 * time.Hour, 50, true, false, Keep},
		{"recent", time.Hour, 50, false, false, Keep},
		{"under cap", 3 * time.Hour, 10000, false, false, Keep},
		{"trim still over cap", 3 * time.Hour, 100, false, true, Remove},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := slotCache(t, tc.idle)
			if tc.largeIncremental {
				for path, size := range map[string]int{"debug/incremental/object": 60, "debug/deps/libx.rlib": 200} {
					file := filepath.Join(cache, filepath.FromSlash(path))
					if err := os.WriteFile(file, make([]byte, size), 0o644); err != nil {
						t.Fatal(err)
					}
					touch(t, time.Now().Add(-tc.idle), file)
				}
			}
			if tc.locked {
				release := holdLock(t, filepath.Join(cache, "debug", ".cargo-lock"))
				defer release()
			}
			opt := cacheOpt
			opt.SlotCap = tc.cap
			cs := caches(t, []string{cache}, nil, opt)
			if c := findCache(t, cs, cache); c.Verdict != tc.want {
				t.Fatalf("got %+v", c)
			}
			done, errs := ApplyCaches(context.Background(), cs)
			if len(errs) != 0 || (len(done) == 1) != (tc.want == Remove) || exists(cache) != (tc.want == Keep) {
				t.Fatalf("done %v, errors %v, cache exists %v", done, errs, exists(cache))
			}
		})
	}
}

func TestGCSlotWithoutIncrementalStaysUnderDiskPressure(t *testing.T) {
	cache := slotCache(t, 3*time.Hour)
	for _, p := range profiles(cache) {
		if err := os.RemoveAll(filepath.Join(p, "incremental")); err != nil {
			t.Fatal(err)
		}
		touch(t, time.Now().Add(-3*time.Hour), p)
	}
	opt := cacheOpt
	opt.FreeBelow = 1000
	opt.Drive = func(string) (Drive, error) { return Drive{ID: "disk", Free: 1}, nil }
	if c := findCache(t, caches(t, []string{cache}, nil, opt), cache); c.Verdict != Keep {
		t.Fatalf("got %+v", c)
	}
}

func TestGCFreeBelowCountsOnlyBytesTrimmed(t *testing.T) {
	cache := slotCache(t, 4*time.Hour)
	other := tagged(t, filepath.Join(tempRoot(t), "other"), 3*time.Hour, 100)
	opt := cacheOpt
	opt.FreeBelow = 50
	opt.Drive = func(string) (Drive, error) { return Drive{ID: "disk", Free: 1}, nil }
	cs := caches(t, []string{cache, other}, nil, opt)
	if c := findCache(t, cs, cache); c.Verdict != Trim {
		t.Fatalf("slot: %+v", c)
	}
	if c := findCache(t, cs, other); c.Verdict != Remove {
		t.Fatalf("other: %+v", c)
	}
}

func TestGCLeavesSlotWhenLockIsTakenAfterRecheck(t *testing.T) {
	cache := slotCache(t, 30*time.Hour)
	cs := caches(t, []string{cache}, nil, cacheOpt)
	var release func()
	beforeTrimLock = func() { release = holdLock(t, filepath.Join(cache, "debug", ".cargo-lock")) }
	defer func() {
		beforeTrimLock = nil
		if release != nil {
			release()
		}
	}()
	done, errs := ApplyCaches(context.Background(), cs)
	if len(done) != 0 || len(errs) == 0 || !exists(filepath.Join(cache, "debug/incremental")) {
		t.Fatalf("done %v errors %v", done, errs)
	}
}

func TestGCRecoversIncrementalMovedAsideAfterFailedDeletion(t *testing.T) {
	cache := slotCache(t, 30*time.Hour)
	cs := caches(t, []string{cache}, nil, cacheOpt)
	original := removeCacheEntry
	removeCacheEntry = func(path string) error {
		if strings.HasSuffix(path, "-incremental") {
			return os.ErrPermission
		}
		return original(path)
	}
	defer func() { removeCacheEntry = original }()
	_, errs := ApplyCaches(context.Background(), cs)
	if len(errs) == 0 {
		t.Fatal("expected deletion error")
	}
	removeCacheEntry = original
	cs = caches(t, []string{cache}, nil, cacheOpt)
	done, errs := ApplyCaches(context.Background(), cs)
	left, _ := filepath.Glob(filepath.Join(cache, "debug", ".gc-*-incremental"))
	if len(errs) != 0 || len(done) != 1 || len(left) != 0 {
		t.Fatalf("done %v errors %v leftovers %v", done, errs, left)
	}
}

func TestGCRecoversLeftoversWithoutTrimmingFreshIncremental(t *testing.T) {
	cache := slotCache(t, time.Hour)
	left := filepath.Join(cache, "debug", ".gc-1-1-incremental")
	if err := os.Mkdir(left, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(left, "old"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	opt := cacheOpt
	opt.FreeBelow = 100
	opt.Drive = func(string) (Drive, error) { return Drive{ID: "disk", Free: 1}, nil }
	cs := caches(t, []string{cache}, nil, opt)
	if c := findCache(t, cs, cache); c.Verdict != Trim || c.TrimSize != 3 || !strings.Contains(c.Reason, "earlier trim") {
		t.Fatalf("got %+v", c)
	}
	done, errs := ApplyCaches(context.Background(), cs)
	if len(errs) != 0 || len(done) != 1 || exists(left) || !exists(filepath.Join(cache, "debug", "incremental", "object")) {
		t.Fatalf("done %v errors %v", done, errs)
	}
}

func TestGCAlsoTrimsLiveOutputWhenTheCapOrLowSpaceSelectsIt(t *testing.T) {
	for _, trigger := range []string{"cap", "low space"} {
		t.Run(trigger, func(t *testing.T) {
			cache := slotCache(t, 3*time.Hour)
			left := filepath.Join(cache, "debug", ".gc-1-1-incremental")
			if err := os.Mkdir(left, 0o755); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(left, "old")
			if err := os.WriteFile(file, []byte("old"), 0o644); err != nil {
				t.Fatal(err)
			}
			at := time.Now().Add(-3 * time.Hour)
			touch(t, at, file, left, filepath.Join(cache, "debug"))
			opt := cacheOpt
			if trigger == "cap" {
				opt.SlotCap = 102
				object := filepath.Join(cache, "debug", "incremental", "object")
				if err := os.WriteFile(object, make([]byte, 60), 0o644); err != nil {
					t.Fatal(err)
				}
				touch(t, at, object)
			} else {
				opt.FreeBelow = 100
				opt.Drive = func(string) (Drive, error) { return Drive{ID: "disk", Free: 1}, nil }
			}
			cs := caches(t, []string{cache}, nil, opt)
			wantSize := int64(23)
			if trigger == "cap" {
				wantSize = 73
			}
			if c := findCache(t, cs, cache); c.Verdict != Trim || c.TrimSize != wantSize {
				t.Fatalf("got %+v", c)
			}
			done, errs := ApplyCaches(context.Background(), cs)
			if len(errs) != 0 || len(done) != 1 || exists(left) || exists(filepath.Join(cache, "debug", "incremental")) {
				t.Fatalf("done %v errors %v", done, errs)
			}
		})
	}
}

func TestGCCanTrimWithReadOnlyCargoLocks(t *testing.T) {
	cache := slotCache(t, 30*time.Hour)
	for _, p := range profiles(cache) {
		lock := filepath.Join(p, ".cargo-lock")
		if err := os.Chmod(lock, 0o444); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(lock, 0o666) })
	}
	cs := caches(t, []string{cache}, nil, cacheOpt)
	if c := findCache(t, cs, cache); c.Verdict != Trim {
		t.Fatalf("got %+v", c)
	}
	done, errs := ApplyCaches(context.Background(), cs)
	if len(errs) != 0 || len(done) != 1 {
		t.Fatalf("done %v errors %v", done, errs)
	}
}

func TestGCKeepsIncrementalWhenMoveFails(t *testing.T) {
	cache := slotCache(t, 30*time.Hour)
	cs := caches(t, []string{cache}, nil, cacheOpt)
	original := renameCache
	renameCache = func(src, dst string) error { return os.ErrPermission }
	defer func() { renameCache = original }()
	done, errs := ApplyCaches(context.Background(), cs)
	if len(done) != 0 || len(errs) == 0 || !strings.Contains(errs[0].Error(), "kept") || !exists(filepath.Join(cache, "debug", "incremental")) {
		t.Fatalf("done %v errors %v", done, errs)
	}
}

func TestGCKeepsSlotWithLinkOutOfIncremental(t *testing.T) {
	cache := slotCache(t, 30*time.Hour)
	outside := t.TempDir()
	link(t, filepath.FromSlash(filepath.Join(cache, "debug", "incremental", "outside")), filepath.FromSlash(outside))
	if c := findCache(t, caches(t, []string{cache}, nil, cacheOpt), cache); c.Verdict != Keep || !strings.Contains(c.Reason, "link out") {
		t.Fatalf("got %+v", c)
	}
}
