package gc

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/report"
)

// removeCacheEntry is replaceable by tests to make an inner deletion fail
// after the cache has been moved aside.
var removeCacheEntry = os.RemoveAll

// movedCachePath is replaceable by tests that need a predictable collision.
var movedCachePath = func(path string) string {
	name := fmt.Sprintf(".gc-%d-%d-%s", os.Getpid(), time.Now().UnixNano(), filepath.Base(path))
	return filepath.Join(filepath.Dir(path), name)
}

// renameCache is replaceable by tests that make a move fail or start a build
// immediately after a successful move.
var renameCache = os.Rename

// movedCacheInUse is replaceable by tests that model a build starting just
// after the move; production probes the moved cache's profile locks.
var movedCacheInUse = inUse

// Cache is one build cache, a folder tagged with CACHEDIR.TAG (Cargo writes
// one into every target folder), and what the cache pass decided about it.
type Cache struct {
	Path       string
	Worktree   string    // the worktree it lies in, "" for one found on its own
	Size       int64     // the files under it
	LastBuild  time.Time // its last write; see lastBuild
	Verdict    Verdict
	Reason     string
	risk       string // why it cannot go whole, found by the walk that sized it
	young      bool   // kept only because it was written too recently
	driveID    string // drive identified during the free-below scan
	driveName  string // readable name from the free-below scan
	driveBelow bool   // that drive was below the limit before any removals
	driveMore  bool   // eligible caches left because enough was already going
}

// Drive is what the cache pass knows about the volume holding a folder.
type Drive struct {
	ID   string // the same for every folder on one volume: the drive on Windows, the device elsewhere
	Name string // readable mount point, when available
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
		if w.Verdict == Remove || !worktreeOf(w.Path, w.Repo) {
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
		var newest time.Time
		c.Size, newest, c.risk = measure(c.Path)
		c.LastBuild = lastBuild(c.Path, newest)
		c.Verdict, c.Reason = decideCache(ctx, c, cwd, now, opt)
	}
	freeBelow(all, now, opt)
	return all, nil
}

// ApplyCaches removes the caches marked Remove, the longest idle first so an
// interruption leaves the newest, each checked again just before it goes
// (still tagged, nothing written since the scan, no build holding it, nothing
// inside that must stay). One that fails the check or the rename stays, with
// the reason among the errors. A failed removal leaves the renamed cache for
// the next scan.
func ApplyCaches(ctx context.Context, caches []Cache) (removed []Cache, errs []error) {
	var todo []Cache
	for _, c := range caches {
		if c.Verdict == Remove {
			todo = append(todo, c)
		}
	}
	sort.SliceStable(todo, func(i, j int) bool { return todo[i].LastBuild.Before(todo[j].LastBuild) })
	for _, c := range todo {
		if why := recheckCache(ctx, c); why != "" {
			errs = append(errs, fmt.Errorf("%s: kept, %s", c.Path, why))
			continue
		}
		moved := movedCachePath(c.Path)
		if err := renameCache(c.Path, moved); err != nil {
			errs = append(errs, fmt.Errorf("%s: kept, it could not be moved aside: %s", c.Path, firstLine(err.Error())))
			continue
		}
		if why := movedCacheInUse(moved); why != "" {
			if err := os.Rename(moved, c.Path); err != nil {
				errs = append(errs, fmt.Errorf("%s (moved to %s): a build started, but it could not be moved back: %w", c.Path, moved, err))
			} else {
				errs = append(errs, fmt.Errorf("%s: kept, a build started: %s", c.Path, why))
			}
			continue
		}
		if err := removeMovedCache(moved); err != nil {
			errs = append(errs, fmt.Errorf("%s (moved to %s): %w", c.Path, moved, err))
			continue
		}
		removed = append(removed, c)
	}
	return removed, errs
}

// removeMovedCache keeps the signature until everything else has gone, so a
// failed deletion remains a tagged cache for the next scan.
func removeMovedCache(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == "CACHEDIR.TAG" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if err := removeCacheEntry(path); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	tag := filepath.Join(dir, "CACHEDIR.TAG")
	if err := os.Remove(tag); err != nil {
		return err
	}
	if err := os.Remove(dir); err != nil {
		if restoreErr := restoreCacheTag(tag); restoreErr != nil {
			return fmt.Errorf("%w (also could not restore %s: %v)", err, tag, restoreErr)
		}
		return err
	}
	return nil
}

func restoreCacheTag(tag string) error {
	f, err := os.OpenFile(tag, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, writeErr := f.WriteString(cacheTagSignature + "\n")
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
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

// worktreeOf reports whether dir is a linked worktree of repo: its .git is a
// file naming a folder under repo's .git/worktrees. git worktree list prints
// whatever an entry's gitdir file says, a path that is no worktree at all
// when the entry is stale or edited, and such a path must not be swept.
func worktreeOf(dir, repo string) bool {
	gitfile := filepath.Join(dir, ".git")
	if info, err := os.Lstat(gitfile); err != nil || !info.Mode().IsRegular() {
		return false
	}
	data, err := os.ReadFile(gitfile)
	if err != nil {
		return false
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return false
	}
	target = filepath.FromSlash(strings.TrimSpace(target))
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	return inside(target, filepath.Join(repo, ".git", "worktrees"))
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
	if isMovedCache(c.Path) {
		return Remove, "moved cache left by an earlier removal"
	}
	idle := ageAt(now, c.LastBuild)
	if idle >= opt.Idle {
		return Remove, "idle " + short(idle)
	}
	c.young = true
	return Keep, fmt.Sprintf("last written %s ago, inside --idle %s", short(idle), short(opt.Idle))
}

// isMovedCache accepts both the old .gc-<pid>-<name> and the newer
// .gc-<pid>-<time>-<name> form. Both start with a decimal PID and a hyphen.
func isMovedCache(dir string) bool {
	name, ok := strings.CutPrefix(filepath.Base(dir), ".gc-")
	if !ok || name == "" {
		return false
	}
	i := 0
	for i < len(name) && name[i] >= '0' && name[i] <= '9' {
		i++
	}
	return i > 0 && i+1 < len(name) && name[i] == '-'
}

// recheckCache says why a cache marked for removal must stay after all,
// because it changed since the scan, or "".
func recheckCache(ctx context.Context, c Cache) string {
	if !isCacheDir(c.Path) {
		return "it is no longer a tagged cache"
	}
	_, newest, risk := measure(c.Path)
	if lastBuild(c.Path, newest).After(c.LastBuild) {
		return "written since the scan"
	}
	if risk != "" {
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
		more        bool
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
		c.driveID = d.ID
		c.driveName = d.Name
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
		alreadyEnough := s.after >= opt.FreeBelow
		sort.SliceStable(s.candidates, func(a, b int) bool { return all[s.candidates[a]].LastBuild.Before(all[s.candidates[b]].LastBuild) })
		for _, i := range s.candidates {
			c := &all[i]
			idle := ageAt(now, c.LastBuild)
			if alreadyEnough {
				if s.free < opt.FreeBelow && idle >= minIdle {
					s.more = true
				}
				continue // enough is going already; keep the original reason
			}
			if idle < minIdle {
				c.Reason = fmt.Sprintf("last written %s ago, inside --min-idle %s", short(idle), short(minIdle))
				continue
			}
			if s.after >= opt.FreeBelow {
				s.more = true
				continue // enough is going already; younger ones keep their reason
			}
			c.Verdict = Remove
			c.Reason = fmt.Sprintf("idle %s, and %s has only %s free, below %s", short(idle), driveLabel(c.driveID, c.driveName), report.Bytes(s.free), report.Bytes(opt.FreeBelow))
			s.after += c.Size
		}
	}
	for i := range all {
		if s := drives[all[i].driveID]; all[i].driveID != "" && s != nil && s.free < opt.FreeBelow {
			all[i].driveBelow = true
			all[i].driveMore = s.more
		}
	}
}

func driveLabel(id, name string) string {
	if name != "" {
		return name
	}
	return id
}

// FreeBelowReport reads the free space after apply for drives that were below
// the limit during the cache scan. Call it only after applying the listing.
func FreeBelowReport(caches []Cache, opt Options) []string {
	if opt.FreeBelow <= 0 {
		return nil
	}
	lookup := opt.Drive
	if lookup == nil {
		lookup = diskFree
	}
	limit := report.Bytes(opt.FreeBelow)
	seen := map[string]bool{}
	var lines []string
	for _, c := range caches {
		if !c.driveBelow || seen[c.driveID] {
			continue
		}
		seen[c.driveID] = true
		label := driveLabel(c.driveID, c.driveName)
		path := c.Path
		for {
			if _, err := os.Stat(path); err == nil {
				break
			}
			parent := filepath.Dir(path)
			if parent == path {
				break
			}
			path = parent
		}
		d, err := lookup(path)
		if err != nil {
			lines = append(lines, fmt.Sprintf("%s free space could not be read: %s", label, firstLine(err.Error())))
			continue
		}
		if d.ID != c.driveID {
			lines = append(lines, fmt.Sprintf("%s free space could not be read: the drive is no longer there", label))
			continue
		}
		if d.Name != "" {
			label = d.Name
		}
		if d.Free >= opt.FreeBelow {
			lines = append(lines, fmt.Sprintf("%s now has %s free, above %s", label, report.Bytes(d.Free), limit))
		} else {
			line := fmt.Sprintf("%s still has %s free, %s below %s", label, report.Bytes(d.Free), report.Bytes(opt.FreeBelow-d.Free), limit)
			if c.driveMore {
				line += "; another hrn gc --free-below run picks the next caches"
			}
			lines = append(lines, line)
		}
	}
	return lines
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

// ageAt is how long before now a cache was last written. A write time a hair
// after now (file times and the clock differ by their resolution) is age 0,
// so --idle 0 takes any cache (#245).
func ageAt(now, written time.Time) time.Duration {
	if d := now.Sub(written); d > 0 {
		return d
	}
	return 0
}

// lastBuild is the newest write among a cache's profile folders, their deps,
// .fingerprint, build and incremental folders, the files directly in a
// profile (the binaries a build ends with), and newest, the latest time the
// walk that sized the cache saw inside it; the cache's own time when it has
// no profile. The folder times catch what a build removes, which no file
// shows; the walk catches a file rewritten deep inside a cache with no
// profile, such as pytest's.
func lastBuild(dir string, newest time.Time) time.Time {
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
// not followed), the newest time of anything in it, and why it cannot be
// deleted whole, or "": a repository inside it, a link leading out of it, or
// a part it cannot read.
func measure(dir string) (size int64, newest time.Time, risk string) {
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
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if t := info.ModTime(); t.After(newest) {
			newest = t
		}
		if d.Type().IsRegular() {
			size += info.Size()
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
	return size, newest, risk
}

// gitTracks reports whether git tracks any file under dir, which makes the
// folder someone's (a tagged test fixture) whatever its tag says. A folder git
// finds no repository above tracks nothing; any other failure is an error, so
// the folder stays. The message is matched in full, because a worktree whose
// repository is gone fails with "not a git repository: <gitdir>", and its
// files are the tracked ones this check protects.
func gitTracks(ctx context.Context, dir string) (bool, error) {
	if _, err := gitOut(ctx, dir, "rev-parse", "--show-toplevel"); err != nil {
		if strings.Contains(err.Error(), "not a git repository (or any") {
			return false, nil
		}
		return false, err
	}
	out, err := gitStdout(ctx, dir, "ls-files", "-z", "--", ".")
	if err != nil {
		return false, err
	}
	return out != "", nil
}
