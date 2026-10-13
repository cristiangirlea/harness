package gc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tagged lays a Cargo target folder out at dir: CACHEDIR.TAG with the
// signature, a debug profile with its .cargo-lock, and deps holding one
// artifact of size bytes, all last written idle ago.
func tagged(t *testing.T, dir string, idle time.Duration, size int) string {
	t.Helper()
	deps := filepath.Join(dir, "debug", "deps")
	if err := os.MkdirAll(deps, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{
		"CACHEDIR.TAG":                              []byte(cacheTagSignature + "\n"),
		filepath.Join("debug", ".cargo-lock"):       nil,
		filepath.Join("debug", "deps", "libx.rlib"): make([]byte, size),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	touch(t, time.Now().Add(-idle), filepath.Join(deps, "libx.rlib"), filepath.Join(dir, "debug", ".cargo-lock"), filepath.Join(dir, "CACHEDIR.TAG"), deps, filepath.Join(dir, "debug"), dir)
	return dir
}

func touch(t *testing.T, at time.Time, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
}

func tempRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

var cacheOpt = Options{Idle: 24 * time.Hour, MinIdle: 2 * time.Hour}

func caches(t *testing.T, paths []string, wts []Worktree, opt Options) []Cache {
	t.Helper()
	cs, err := Caches(context.Background(), paths, wts, opt)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func findCache(t *testing.T, cs []Cache, path string) Cache {
	t.Helper()
	for _, c := range cs {
		if samePath(c.Path) == samePath(path) {
			return c
		}
	}
	t.Fatalf("%s is not among the caches: %+v", path, cs)
	return Cache{}
}

func wantCache(t *testing.T, c Cache, v Verdict, reason string) {
	t.Helper()
	if c.Verdict != v || !strings.Contains(c.Reason, reason) {
		t.Errorf("%s: got %s (%s), want %s with a reason containing %q", filepath.Base(c.Path), c.Verdict, c.Reason, v, reason)
	}
}

func TestGCRemovesATaggedCacheIdlePastTheLimit(t *testing.T) {
	root := tempRoot(t)
	old := tagged(t, filepath.Join(root, "old-target"), 30*time.Hour, 100)

	cs := caches(t, []string{root}, nil, cacheOpt)
	c := findCache(t, cs, old)
	wantCache(t, c, Remove, "idle 30h")
	if c.Size < 100 || c.LastBuild.IsZero() {
		t.Errorf("a cache carries its size and last build: %+v", c)
	}
	removed, errs := ApplyCaches(context.Background(), cs)
	if len(errs) != 0 || len(removed) != 1 || exists(old) {
		t.Errorf("apply: removed %d, errors %v, folder still there: %v", len(removed), errs, exists(old))
	}
}

func TestGCKeepsACacheWrittenRecently(t *testing.T) {
	root := tempRoot(t)
	fresh := tagged(t, filepath.Join(root, "fresh"), time.Hour, 100)

	cs := caches(t, []string{root}, nil, cacheOpt)
	wantCache(t, findCache(t, cs, fresh), Keep, "inside --idle 24h")
	if _, errs := ApplyCaches(context.Background(), cs); len(errs) != 0 || !exists(fresh) {
		t.Errorf("a kept cache must survive apply: %v", errs)
	}
}

func TestGCSeesAWriteDeepInsideACacheWithoutProfiles(t *testing.T) {
	root := tempRoot(t)
	pytest := filepath.Join(root, ".pytest_cache")
	deep := filepath.Join(pytest, "v", "cache")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"CACHEDIR.TAG": cacheTagSignature + "\n", filepath.Join("v", "cache", "lastfailed"): "{}\n"} {
		if err := os.WriteFile(filepath.Join(pytest, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-30 * time.Hour)
	touch(t, old, filepath.Join(pytest, "CACHEDIR.TAG"), deep, filepath.Join(pytest, "v"), pytest) // every folder is old; the one file is fresh

	cs := caches(t, []string{root}, nil, cacheOpt)
	wantCache(t, findCache(t, cs, pytest), Keep, "inside --idle 24h")
}

func TestGCKeepsACacheWhenGitCannotSayWhetherItTracksIt(t *testing.T) {
	root := tempRoot(t)
	broken := filepath.Join(root, "broken")
	cache := tagged(t, filepath.Join(broken, "target"), 30*time.Hour, 10)
	if err := os.WriteFile(filepath.Join(broken, ".git"), []byte("not a gitdir line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cs := caches(t, []string{root}, nil, cacheOpt)
	wantCache(t, findCache(t, cs, cache), Keep, "git could not say whether it tracks files in it")
}

func TestGCKeepsACacheInAWorktreeWhoseRepositoryIsGone(t *testing.T) {
	root := tempRoot(t)
	orphan := filepath.Join(root, "wt")
	fixture := tagged(t, filepath.Join(orphan, "fixtures"), 30*time.Hour, 10)
	if err := os.WriteFile(filepath.Join(orphan, ".git"), []byte("gitdir: "+filepath.ToSlash(root)+"/gone/.git/worktrees/wt\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cs := caches(t, []string{root}, nil, cacheOpt)
	wantCache(t, findCache(t, cs, fixture), Keep, "git could not say whether it tracks files in it")
}

func TestGCApplyRemovesTheLongestIdleCacheFirst(t *testing.T) {
	root := tempRoot(t)
	newer := tagged(t, filepath.Join(root, "a-newer"), 30*time.Hour, 10)
	older := tagged(t, filepath.Join(root, "z-older"), 60*time.Hour, 10)

	removed, errs := ApplyCaches(context.Background(), caches(t, []string{root}, nil, cacheOpt))
	if len(errs) != 0 || len(removed) != 2 {
		t.Fatalf("removed %d, errors %v", len(removed), errs)
	}
	if samePath(removed[0].Path) != samePath(older) || samePath(removed[1].Path) != samePath(newer) {
		t.Errorf("removal order %q, %q; want the older first", removed[0].Path, removed[1].Path)
	}
}

func TestDiskFreeNamesTheVolumeAndItsFreeSpace(t *testing.T) {
	root := tempRoot(t)
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := diskFree(root)
	if err != nil || d.ID == "" || d.Free <= 0 {
		t.Fatalf("diskFree(%s) = %+v, %v", root, d, err)
	}
	if d2, err := diskFree(sub); err != nil || d2.ID != d.ID {
		t.Errorf("a folder on the same volume has another id: %+v, %v (want %q)", d2, err, d.ID)
	}
}

func TestGCTakesTheIdleLimitAsGiven(t *testing.T) {
	root := tempRoot(t)
	fresh := tagged(t, filepath.Join(root, "fresh"), 0, 100)

	cs := caches(t, []string{root}, nil, Options{Idle: time.Nanosecond, MinIdle: time.Nanosecond})
	wantCache(t, findCache(t, cs, fresh), Remove, "idle")
}

func TestGCKeepsACacheWhoseBuildHoldsTheLock(t *testing.T) {
	root := tempRoot(t)
	busy := tagged(t, filepath.Join(root, "busy"), 30*time.Hour, 100)
	release := holdLock(t, filepath.Join(busy, "debug", ".cargo-lock"))
	defer release()

	cs := caches(t, []string{root}, nil, cacheOpt)
	wantCache(t, findCache(t, cs, busy), Keep, "in use: debug/.cargo-lock is held")
}

func TestGCNeverTouchesAnUntaggedFolder(t *testing.T) {
	root := tempRoot(t)
	untagged := tagged(t, filepath.Join(root, "target"), 30*time.Hour, 100)
	if err := os.Remove(filepath.Join(untagged, "CACHEDIR.TAG")); err != nil {
		t.Fatal(err)
	}
	unsigned := tagged(t, filepath.Join(root, "notes"), 30*time.Hour, 100)
	if err := os.WriteFile(filepath.Join(unsigned, "CACHEDIR.TAG"), []byte("not the signature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	touch(t, time.Now().Add(-30*time.Hour), untagged, unsigned)

	cs := caches(t, []string{root}, nil, cacheOpt)
	if len(cs) != 0 {
		t.Fatalf("an untagged or unsigned folder is not a cache: %+v", cs)
	}
}

func TestGCFreeBelowRemovesTheOldestCachesFirstUntilTheDriveHasEnough(t *testing.T) {
	root := tempRoot(t)
	oldest := tagged(t, filepath.Join(root, "c10h"), 10*time.Hour, 3000)
	older := tagged(t, filepath.Join(root, "c5h"), 5*time.Hour, 2000)
	recent := tagged(t, filepath.Join(root, "c3h"), 3*time.Hour, 1000)
	young := tagged(t, filepath.Join(root, "c1h"), time.Hour, 1000)
	elsewhere := tagged(t, filepath.Join(root, "other-drive"), 10*time.Hour, 1000)

	opt := cacheOpt
	opt.Drive = func(path string) (Drive, error) {
		if samePath(path) == samePath(elsewhere) {
			return Drive{ID: "Y:", Free: 1 << 40}, nil
		}
		return Drive{ID: "X:", Free: 100}, nil
	}
	opt.FreeBelow = 100 + Size(oldest) + 1 // the oldest alone is not enough, the two oldest are

	cs := caches(t, []string{root}, nil, opt)
	wantCache(t, findCache(t, cs, oldest), Remove, "X: has only 100 bytes free")
	wantCache(t, findCache(t, cs, older), Remove, "X: has only 100 bytes free")
	wantCache(t, findCache(t, cs, recent), Keep, "inside --idle 24h")
	wantCache(t, findCache(t, cs, young), Keep, "inside --min-idle 2h")
	wantCache(t, findCache(t, cs, elsewhere), Keep, "inside --idle 24h")
}

func TestGCFreeBelowLeavesADriveWhoseFreeSpaceIsUnknown(t *testing.T) {
	root := tempRoot(t)
	c := tagged(t, filepath.Join(root, "c10h"), 10*time.Hour, 1000)
	opt := cacheOpt
	opt.FreeBelow = 1 << 40
	opt.Drive = func(string) (Drive, error) { return Drive{}, os.ErrPermission }

	cs := caches(t, []string{root}, nil, opt)
	wantCache(t, findCache(t, cs, c), Keep, "free space unknown")
}

func TestGCFindsCachesInsideWorktreesButNotThoseGoingWithTheirWorktree(t *testing.T) {
	f := newFixture(t)
	kept, h1 := f.worktree("wt-open", "open")
	f.pr(1, "open", "OPEN", h1, 0)
	going, h2 := f.worktree("wt-done", "done")
	f.land("done")
	f.pr(2, "done", "MERGED", h2, 2*time.Hour)
	inKept := tagged(t, filepath.Join(kept, "target"), 30*time.Hour, 10)
	tagged(t, filepath.Join(going, "target"), 30*time.Hour, 10)

	wts := f.scan()
	wantVerdict(t, find(t, wts, going), Remove, "PR #2 merged")
	cs := caches(t, []string{f.repo}, wts, cacheOpt)
	if len(cs) != 1 {
		t.Fatalf("want the kept worktree's cache alone, got %+v", cs)
	}
	c := findCache(t, cs, inKept)
	wantCache(t, c, Remove, "idle 30h")
	if samePath(c.Worktree) != samePath(kept) {
		t.Errorf("the cache names its worktree: got %q, want %q", c.Worktree, kept)
	}
}

func TestGCSweepsNoCachesUnderAWorktreeEntryThatIsNoWorktree(t *testing.T) {
	f := newFixture(t)
	elsewhere := filepath.Join(f.root, "elsewhere")
	cache := tagged(t, filepath.Join(elsewhere, "target"), 30*time.Hour, 10)
	entry := filepath.Join(f.repo, ".git", "worktrees", "bogus") // a stale or edited entry pointing at a folder that is no worktree
	if err := os.MkdirAll(entry, 0o755); err != nil {
		t.Fatal(err)
	}
	f.write(entry, "gitdir", filepath.ToSlash(elsewhere)+"/.git\n")
	f.write(entry, "HEAD", git(t, f.repo, "rev-parse", "HEAD")+"\n")
	f.write(entry, "commondir", "../..\n")

	wts := f.scan()
	wantVerdict(t, find(t, wts, elsewhere), Keep, "")
	for _, c := range caches(t, []string{f.repo}, wts, cacheOpt) {
		if samePath(c.Path) == samePath(cache) {
			t.Fatalf("a folder that is no worktree of the repository was swept: %+v", c)
		}
	}
}

func TestGCListsACacheFoundBothAsAPathAndInAWorktreeOnce(t *testing.T) {
	f := newFixture(t)
	wt, h := f.worktree("wt-open", "open")
	f.pr(1, "open", "OPEN", h, 0)
	cache := tagged(t, filepath.Join(wt, "target"), 30*time.Hour, 10)

	cs := caches(t, []string{f.root}, f.scan(), cacheOpt)
	n := 0
	for _, c := range cs {
		if samePath(c.Path) == samePath(cache) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the worktree's cache is listed %d times, want once: %+v", n, cs)
	}
}

func TestGCKeepsACacheGitTracksFilesIn(t *testing.T) {
	f := newFixture(t)
	fixture := tagged(t, filepath.Join(f.repo, "fixtures"), 30*time.Hour, 10)
	git(t, f.repo, "add", "-f", "fixtures")
	git(t, f.repo, "commit", "-q", "-m", "a tagged test fixture")

	cs := caches(t, []string{f.repo}, nil, cacheOpt)
	wantCache(t, findCache(t, cs, fixture), Keep, "git tracks files in it")
}

func TestGCKeepsACacheHoldingALinkOutOrARepository(t *testing.T) {
	root := tempRoot(t)
	outside := filepath.Join(root, "store")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	linked := tagged(t, filepath.Join(root, "linked"), 30*time.Hour, 10)
	link(t, filepath.Join(linked, "debug", "deps", "shared"), outside)
	hosting := tagged(t, filepath.Join(root, "hosting"), 30*time.Hour, 10)
	git(t, hosting, "init", "-q", filepath.Join(hosting, "debug", "build", "vendored"))
	touch(t, time.Now().Add(-30*time.Hour), filepath.Join(linked, "debug", "deps"), filepath.Join(hosting, "debug"))

	cs := caches(t, []string{root}, nil, cacheOpt)
	wantCache(t, findCache(t, cs, linked), Keep, "a link out of the cache inside it: debug/deps/shared")
	wantCache(t, findCache(t, cs, hosting), Keep, "a repository inside it: debug/build/vendored")
}

func TestGCKeepsTheCacheItRunsIn(t *testing.T) {
	root := tempRoot(t)
	here := tagged(t, filepath.Join(root, "here"), 30*time.Hour, 10)
	t.Chdir(filepath.Join(here, "debug"))

	cs := caches(t, []string{root}, nil, cacheOpt)
	wantCache(t, findCache(t, cs, here), Keep, "the current directory is inside it")
}

func TestGCApplyLeavesACacheWrittenOrLockedSinceTheScan(t *testing.T) {
	root := tempRoot(t)
	written := tagged(t, filepath.Join(root, "written"), 30*time.Hour, 10)
	locked := tagged(t, filepath.Join(root, "locked"), 30*time.Hour, 10)
	cs := caches(t, []string{root}, nil, cacheOpt)
	wantCache(t, findCache(t, cs, written), Remove, "idle")
	wantCache(t, findCache(t, cs, locked), Remove, "idle")

	touch(t, time.Now(), filepath.Join(written, "debug", "deps"))
	release := holdLock(t, filepath.Join(locked, "debug", ".cargo-lock"))
	defer release()

	removed, errs := ApplyCaches(context.Background(), cs)
	if len(removed) != 0 || !exists(written) || !exists(locked) {
		t.Fatalf("removed %d caches, written there: %v, locked there: %v", len(removed), exists(written), exists(locked))
	}
	all := ""
	for _, err := range errs {
		all += err.Error() + "\n"
	}
	for _, want := range []string{"written since the scan", "in use: debug/.cargo-lock is held"} {
		if !strings.Contains(all, want) {
			t.Errorf("errors lack %q:\n%s", want, all)
		}
	}
}

func TestTotalCachesCountsOneInsideAnotherOnce(t *testing.T) {
	root := t.TempDir()
	outer, inner, other := filepath.Join(root, "a"), filepath.Join(root, "a", "b"), filepath.Join(root, "c")
	cs := []Cache{{Path: outer, Size: 10}, {Path: inner, Size: 4}, {Path: other, Size: 1}}
	if got := TotalCaches(cs); got != 11 {
		t.Errorf("TotalCaches = %d, want 11", got)
	}
}
