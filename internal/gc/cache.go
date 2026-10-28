package gc

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/report"
)

var removeCacheEntry = os.RemoveAll

var movedCachePath = func(path string) string {
	name := fmt.Sprintf(".gc-%d-%d-%s", os.Getpid(), time.Now().UnixNano(), filepath.Base(path))
	return filepath.Join(filepath.Dir(path), name)
}

var renameCache = os.Rename

var movedCacheInUse = inUse

var beforeTrimLock func()

// Cache is a CACHEDIR.TAG-tagged build folder and its removal verdict.
type Cache struct {
	Path        string
	Worktree    string // empty for a standalone cache
	Size        int64  // bytes
	TrimSize    int64
	LastBuild   time.Time // includes deletions; see lastBuild
	Verdict     Verdict
	Reason      string
	risk        string // why whole-folder deletion is unsafe
	young       bool   // age is the only reason to keep it
	driveID     string
	driveName   string
	driveBelow  bool // before removals
	driveMore   bool // eligible caches remain after meeting the limit
	slot        bool
	incremental bool
	leftovers   bool
}

// Drive describes a cache's volume and available space.
type Drive struct {
	ID   string // shared by all folders on the volume
	Name string // mount point, when available
	Free int64  // bytes free for this user
}

// Caches judges tagged folders up to two levels below paths and retained worktrees.
func Caches(ctx context.Context, paths []string, wts []Worktree, opt Options) ([]Cache, error) {
	cwd, _ := os.Getwd()
	seen := map[string]bool{}
	var all []Cache
	add := func(dir string) {
		if k := samePath(dir); !seen[k] {
			seen[k] = true
			all = append(all, Cache{Path: dir, Worktree: holder(dir, wts), slot: slotCachePath(dir) && len(profiles(dir)) > 0})
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
		if c.slot {
			c.TrimSize, c.incremental, c.leftovers = incrementalSize(c.Path)
		}
		c.Verdict, c.Reason = decideCache(ctx, c, cwd, now, opt)
	}
	freeBelow(all, now, opt)
	return all, nil
}

// ApplyCaches rechecks and applies cache verdicts, oldest first so interruptions spare newer caches.
// Failed deletions leave renamed, tagged caches for the next scan.
func ApplyCaches(ctx context.Context, caches []Cache) (removed []Cache, errs []error) {
	var todo []Cache
	for _, c := range caches {
		if c.Verdict == Remove || c.Verdict == Trim {
			todo = append(todo, c)
		}
	}
	sort.SliceStable(todo, func(i, j int) bool { return todo[i].LastBuild.Before(todo[j].LastBuild) })
	for _, c := range todo {
		if why := recheckCache(ctx, c); why != "" {
			errs = append(errs, fmt.Errorf("%s: kept, %s", c.Path, why))
			continue
		}
		if c.Verdict == Trim {
			failed := false
			if beforeTrimLock != nil {
				beforeTrimLock()
			}
			ps := profiles(c.Path)
			var unlocks []func()
			for _, p := range ps {
				unlock, err := holdCargoLock(filepath.Join(p, ".cargo-lock"))
				if err != nil {
					errs = append(errs, fmt.Errorf("%s: kept, lock could not be acquired: %w", c.Path, err))
					failed = true
					break
				}
				unlocks = append(unlocks, unlock)
			}
			if !failed && strings.Join(profiles(c.Path), "\x00") != strings.Join(ps, "\x00") {
				errs = append(errs, fmt.Errorf("%s: kept, profiles changed since locking", c.Path))
				failed = true
			}
			if !failed {
				if why := recheckCacheFiles(ctx, c); why != "" {
					errs = append(errs, fmt.Errorf("%s: kept, %s", c.Path, why))
					failed = true
				}
			}
			var moved []string
			for _, p := range ps {
				if failed {
					break
				}
				for _, path := range incrementalPaths(p) {
					if filepath.Base(path) == "incremental" {
						dest := movedCachePath(path)
						if err := renameCache(path, dest); err != nil {
							errs = append(errs, fmt.Errorf("%s: kept, incremental could not be moved aside: %w", c.Path, err))
							failed = true
							break
						}
						path = dest
					}
					moved = append(moved, path)
				}
			}
			for _, path := range moved {
				if failed {
					break
				}
				if err := removeCacheEntry(path); err != nil {
					errs = append(errs, fmt.Errorf("%s: trim left %s: %w", c.Path, path, err))
					failed = true
					break
				}
			}
			for _, unlock := range unlocks {
				unlock()
			}
			if !failed {
				removed = append(removed, c)
			}
			continue
		}
		moved := movedCachePath(c.Path)
		if err := renameCache(c.Path, moved); err != nil {
			errs = append(errs, fmt.Errorf("%s: kept, it could not be moved aside: %s", c.Path, firstLine(err.Error())))
			continue
		}
		// A build can take a lock between the recheck and the move.
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

// Keep the tag until last so failed deletions remain discoverable caches.
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

// Validate the backlink: stale or edited gitdir entries can name unrelated folders.
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

func holder(dir string, wts []Worktree) string {
	best := ""
	for _, w := range wts {
		if inside(dir, w.Path) && len(w.Path) > len(best) {
			best = w.Path
		}
	}
	return best
}

func goesWithWorktree(dir string, wts []Worktree) bool {
	for _, w := range wts {
		if w.Verdict == Remove && inside(dir, w.Path) {
			return true
		}
	}
	return false
}

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
	if c.slot {
		if c.leftovers || (c.incremental && idle >= opt.Idle) {
			return Trim, "idle " + short(idle)
		}
		if c.incremental && opt.SlotCap > 0 && c.Size > opt.SlotCap && c.TrimSize >= opt.SlotCap/2+opt.SlotCap%2 && idle >= opt.MinIdle {
			return Trim, fmt.Sprintf("idle %s, over --slot-cap %s", short(idle), report.Bytes(opt.SlotCap))
		}
		c.young = c.incremental
		return Keep, "slot cache retained"
	}
	if idle >= opt.Idle {
		return Remove, "idle " + short(idle)
	}
	c.young = true
	return Keep, fmt.Sprintf("last written %s ago, inside --idle %s", short(idle), short(opt.Idle))
}

func slotCachePath(path string) bool {
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		parent := filepath.Dir(p)
		name := filepath.Base(p)
		slots, hrn := filepath.Base(parent), filepath.Base(filepath.Dir(parent))
		match := slots == "slots" && hrn == ".hrn"
		if runtime.GOOS == "windows" {
			match = strings.EqualFold(slots, "slots") && strings.EqualFold(hrn, ".hrn")
		}
		if n, err := strconv.Atoi(name); err == nil && n > 0 && strconv.Itoa(n) == name && match {
			return true
		}
		if parent == p {
			return false
		}
	}
}

func incrementalSize(dir string) (int64, bool, bool) {
	var total int64
	var found, leftovers bool
	for _, p := range profiles(dir) {
		for _, path := range incrementalPaths(p) {
			if filepath.Base(path) == "incremental" {
				found = true
			} else {
				leftovers = true
			}
			size, _, _ := measure(path)
			total += size
		}
	}
	return total, found, leftovers
}

func incrementalPaths(profile string) []string {
	entries, err := os.ReadDir(profile)
	if err != nil {
		return nil
	}
	var paths []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() && (name == "incremental" || (strings.HasSuffix(name, "-incremental") && isMovedCache(name))) {
			paths = append(paths, filepath.Join(profile, name))
		}
	}
	return paths
}

// Accept both on-disk forms: .gc-<pid>-<name> and .gc-<pid>-<time>-<name>.
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

// Recheck before deletion: writes, locks or tracked files may have appeared since scanning.
func recheckCache(ctx context.Context, c Cache) string {
	if why := recheckCacheFiles(ctx, c); why != "" {
		return why
	}
	return inUse(c.Path)
}

func recheckCacheFiles(ctx context.Context, c Cache) string {
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
	return ""
}

// Unknown free space must not justify extra removals.
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
		case c.Verdict == Trim:
			s.after += c.TrimSize
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
				continue
			}
			if idle < minIdle {
				c.Reason = fmt.Sprintf("last written %s ago, inside --min-idle %s", short(idle), short(minIdle))
				continue
			}
			if s.after >= opt.FreeBelow {
				s.more = true
				continue
			}
			if c.slot {
				if !c.incremental {
					continue
				}
				c.Verdict = Trim
			} else {
				c.Verdict = Remove
			}
			c.Reason = fmt.Sprintf("idle %s, and %s has only %s free, below %s", short(idle), driveLabel(c.driveID, c.driveName), report.Bytes(s.free), report.Bytes(opt.FreeBelow))
			if c.slot {
				s.after += c.TrimSize
			} else {
				s.after += c.Size
			}
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

// FreeBelowReport reports space on previously low drives after the listing has been applied.
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

// Clamp clock-resolution skew to zero so --idle 0 includes future timestamps.
func ageAt(now, written time.Time) time.Duration {
	if d := now.Sub(written); d > 0 {
		return d
	}
	return 0
}

// Directory times catch build deletions; newest catches deep rewrites in caches without profiles.
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

// An unreadable lock counts as held: absence of a build must be proven.
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

func measure(dir string) (size int64, newest time.Time, risk string) {
	var kind, at string
	unreadable := false
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			unreadable = true
			return nil
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

// A cache tag does not make tracked fixtures disposable.
// Match the no-parent-repository message narrowly: a missing worktree repository must keep its files.
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
