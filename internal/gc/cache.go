package gc

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/cristiangirlea/harness/internal/report"
)

// Cache is one build cache, a folder tagged with CACHEDIR.TAG (Cargo writes
// one into every target folder), and what the cache pass decided about it.
type Cache struct {
	Path      string
	Worktree  string    // the worktree it lies in, "" for one found on its own
	Size      int64     // the files under it
	LastBuild time.Time // its last write; see lastBuild
	Verdict   Verdict
	Reason    string
	risk      string // why it cannot go whole, found by the walk that sized it
	young     bool   // kept only because it was written too recently
}

// Drive is what the cache pass knows about the volume holding a folder.
type Drive struct {
	ID   string // the same for every folder on one volume: the drive on Windows, the device elsewhere
	Free int64  // bytes free for this user
}

// Caches lists the build caches at paths, up to two levels down, and inside
// each worktree that stays, up to two levels down, once each, with a verdict.
// A cache goes when no build holds it and it was last written more than
// opt.Idle ago; when the drive holding it has less than opt.FreeBelow free,
// the caches on that drive idle past opt.MinIdle go too, oldest first, until
// the drive would have enough. One inside a worktree marked Remove goes with
// the worktree and is not listed.
func Caches(ctx context.Context, paths []string, wts []Worktree, opt Options) ([]Cache, error) {
	cwd, _ := os.Getwd()
	seen := map[string]bool{}
	var all []Cache
	add := func(dir string) {
		if k := samePath(dir); !seen[k] {
			seen[k] = true
			all = append(all, Cache{Path: dir, Worktree: holder(dir, wts)})
		}
	}
	for _, w := range wts {
		if w.Verdict == Remove {
			continue
		}
		for _, dir := range cacheDirs(w.Path, 2) {
			add(dir)
		}
	}
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		if info, err := os.Stat(abs); err != nil {
			return nil, err
		} else if !info.IsDir() {
			return nil, fmt.Errorf("%s is not a directory", abs)
		}
		for _, dir := range cacheDirs(abs, 2) {
			if goesWithWorktree(dir, wts) {
				continue
			}
			add(dir)
		}
	}
	now := time.Now()
	for i := range all {
		c := &all[i]
		c.Size, c.risk = measure(c.Path)
		c.LastBuild = lastBuild(c.Path)
		c.Verdict, c.Reason = decideCache(ctx, c, cwd, now, opt)
	}
	freeBelow(all, now, opt)
	return all, nil
}

// ApplyCaches removes the caches marked Remove, each checked again just
// before it goes (still tagged, nothing written since the scan, no build
// holding it, nothing inside that must stay). One that fails the check stays,
// with the reason among the errors. The removal is not all-or-nothing: a file
// held open on Windows can leave part of the folder deleted.
func ApplyCaches(ctx context.Context, caches []Cache) (removed []Cache, errs []error) {
	for _, c := range caches {
		if c.Verdict != Remove {
			continue
		}
		if why := recheckCache(ctx, c); why != "" {
			errs = append(errs, fmt.Errorf("%s: kept, %s", c.Path, why))
			continue
		}
		if err := os.RemoveAll(c.Path); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.Path, err))
			continue
		}
		removed = append(removed, c)
	}
	return removed, errs
}

// TotalCaches adds up the caches' sizes, counting one inside another once.
func TotalCaches(caches []Cache) int64 {
	paths := make([]string, len(caches))
	for i, c := range caches {
		paths[i] = c.Path
	}
	var n int64
	for _, c := range caches {
		if !nested(c.Path, paths) {
			n += c.Size
		}
	}
	return n
}

// cacheDirs finds the tagged folders at dir and up to depth levels down,
// without looking inside one, through a link, or into .git.
func cacheDirs(dir string, depth int) []string {
	if isCacheDir(dir) {
		return []string{dir}
	}
	if depth == 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var found []string
	for _, e := range entries {
		if !e.IsDir() || e.Name() == ".git" { // a link or a junction is not a directory to ReadDir
			continue
		}
		found = append(found, cacheDirs(filepath.Join(dir, e.Name()), depth-1)...)
	}
	return found
}

// holder names the innermost worktree dir lies in, or "".
func holder(dir string, wts []Worktree) string {
	best := ""
	for _, w := range wts {
		if inside(dir, w.Path) && len(w.Path) > len(best) {
			best = w.Path
		}
	}
	return best
}

// goesWithWorktree reports a folder inside a worktree marked Remove: the
// worktree's removal takes it.
func goesWithWorktree(dir string, wts []Worktree) bool {
	for _, w := range wts {
		if w.Verdict == Remove && inside(dir, w.Path) {
			return true
		}
	}
	return false
}

// decideCache gives one cache its verdict: keep, with the first reason that
// holds it, or remove, because nothing has written it for opt.Idle.
func decideCache(ctx context.Context, c *Cache, cwd string, now time.Time, opt Options) (Verdict, string) {
	if cwd != "" && inside(cwd, c.Path) {
		return Keep, "the current directory is inside it"
	}
	if c.risk != "" {
		return Keep, c.risk
	}
	tracked, err := gitTracks(ctx, c.Path)
	switch {
	case err != nil:
		return Keep, "git could not say whether it tracks files in it: " + firstLine(err.Error())
	case tracked:
		return Keep, "git tracks files in it"
	}
	if why := inUse(c.Path); why != "" {
		return Keep, why
	}
	idle := now.Sub(c.LastBuild)
	if idle >= opt.Idle {
		return Remove, "idle " + short(idle)
	}
	c.young = true
	return Keep, fmt.Sprintf("last written %s ago, inside --idle %s", short(idle), short(opt.Idle))
}

// recheckCache says why a cache marked for removal must stay after all,
// because it changed since the scan, or "".
func recheckCache(ctx context.Context, c Cache) string {
	if !isCacheDir(c.Path) {
		return "it is no longer a tagged cache"
	}
	if lastBuild(c.Path).After(c.LastBuild) {
		return "written since the scan"
	}
	if _, risk := measure(c.Path); risk != "" {
		return risk
	}
	tracked, err := gitTracks(ctx, c.Path)
	switch {
	case err != nil:
		return "git could not say whether it tracks files in it: " + firstLine(err.Error())
	case tracked:
		return "git tracks files in it"
	}
	return inUse(c.Path)
}

// freeBelow marks more caches for removal on each drive with less than
// opt.FreeBelow free: those kept only for being young, idle past opt.MinIdle,
// oldest first, until what goes would put the drive above the limit. A drive
// whose free space cannot be read loses nothing more.
func freeBelow(all []Cache, now time.Time, opt Options) {
	if opt.FreeBelow <= 0 {
		return
	}
	minIdle := opt.MinIdle
	lookup := opt.Drive
	if lookup == nil {
		lookup = diskFree
	}
	type drive struct {
		free, after int64 // free now, and once what is marked has gone
		candidates  []int
	}
	drives := map[string]*drive{}
	var order []string
	for i := range all {
		c := &all[i]
		d, err := lookup(c.Path)
		if err != nil {
			if c.young {
				c.Reason += "; free space unknown: " + firstLine(err.Error())
			}
			continue
		}
		s := drives[d.ID]
		if s == nil {
			s = &drive{free: d.Free, after: d.Free}
			drives[d.ID] = s
			order = append(order, d.ID)
		}
		switch {
		case c.Verdict == Remove:
			s.after += c.Size
		case c.young:
			s.candidates = append(s.candidates, i)
		}
	}
	for _, id := range order {
		s := drives[id]
		if s.after >= opt.FreeBelow {
			continue
		}
		sort.SliceStable(s.candidates, func(a, b int) bool { return all[s.candidates[a]].LastBuild.Before(all[s.candidates[b]].LastBuild) })
		for _, i := range s.candidates {
			c := &all[i]
			idle := now.Sub(c.LastBuild)
			if idle < minIdle {
				c.Reason = fmt.Sprintf("last written %s ago, inside --min-idle %s", short(idle), short(minIdle))
				continue
			}
			if s.after >= opt.FreeBelow {
				continue // enough is going already; younger ones keep their reason
			}
			c.Verdict = Remove
			c.Reason = fmt.Sprintf("idle %s, and %s has only %s free, below %s", short(idle), id, report.Bytes(s.free), report.Bytes(opt.FreeBelow))
			s.after += c.Size
		}
	}
}

// profiles lists a cache's profile folders, those holding Cargo's .cargo-lock
// (debug, release, a custom profile, or the same under a target triple), up
// to two levels down.
func profiles(dir string) []string {
	var out []string
	var walk func(d string, depth int)
	walk = func(d string, depth int) {
		entries, err := os.ReadDir(d)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			sub := filepath.Join(d, e.Name())
			if info, err := os.Lstat(filepath.Join(sub, ".cargo-lock")); err == nil && info.Mode().IsRegular() {
				out = append(out, sub)
			} else if depth > 1 {
				walk(sub, depth-1)
			}
		}
	}
	walk(dir, 2)
	sort.Strings(out)
	return out
}

// lastBuild is the newest write among a cache's profile folders, their deps,
// .fingerprint, build and incremental folders, and the files directly in a
// profile (the binaries a build ends with); the cache's own time when it has
// no profile. Folder times move when entries come and go, which a build does,
// so the hundreds of thousands of files inside are not walked.
func lastBuild(dir string) time.Time {
	var newest time.Time
	note := func(p string) {
		if info, err := os.Lstat(p); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	ps := profiles(dir)
	for _, p := range ps {
		note(p)
		for _, sub := range []string{"deps", ".fingerprint", "build", "incremental"} {
			note(filepath.Join(p, sub))
		}
		if entries, err := os.ReadDir(p); err == nil {
			for _, e := range entries {
				if e.Type().IsRegular() {
					note(filepath.Join(p, e.Name()))
				}
			}
		}
	}
	if len(ps) == 0 {
		note(dir)
	}
	return newest
}

// inUse says why a cache is in use, or "": a build holds a profile's
// .cargo-lock (Cargo keeps it while it builds), or the lock could not be
// checked, which counts as held.
func inUse(dir string) string {
	for _, p := range profiles(dir) {
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		held, err := lockHeld(filepath.Join(p, ".cargo-lock"))
		switch {
		case err != nil:
			return fmt.Sprintf("the lock %s/.cargo-lock could not be checked: %v", rel, err)
		case held:
			return "in use: " + rel + "/.cargo-lock is held"
		}
	}
	return ""
}

// measure walks a cache once for the size of the files under it (links are
// not followed) and for why it cannot be deleted whole, or "": a repository
// inside it, a link leading out of it, or a part it cannot read.
func measure(dir string) (size int64, risk string) {
	var kind, at string
	unreadable := false
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			unreadable = true
			return nil // what could be read still counts
		}
		if kind == "" {
			kind, at = riskAt(dir, p, d)
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				size += info.Size()
			}
		}
		return nil
	})
	rel := func(p string) string {
		r, _ := filepath.Rel(dir, p)
		return filepath.ToSlash(r)
	}
	switch {
	case kind == riskRepo:
		risk = "a repository inside it: " + rel(at)
	case kind == riskLink:
		risk = "a link out of the cache inside it: " + rel(at)
	case unreadable:
		risk = "a part of it cannot be read"
	}
	return size, risk
}

// gitTracks reports whether git tracks any file under dir, which makes the
// folder someone's (a tagged test fixture) whatever its tag says. A folder in
// no repository tracks nothing.
func gitTracks(ctx context.Context, dir string) (bool, error) {
	if _, err := gitOut(ctx, dir, "rev-parse", "--show-toplevel"); err != nil {
		return false, nil
	}
	out, err := gitStdout(ctx, dir, "ls-files", "-z", "--", ".")
	if err != nil {
		return false, err
	}
	return out != "", nil
}
