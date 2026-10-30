// Package gc removes worktrees only with proof their work merged; uncertainty keeps them.
package gc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/gitretry"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// Verdict is the removal decision: "remove" or "keep".
type Verdict string

const (
	Remove Verdict = "remove"
	Keep   Verdict = "keep"
	Trim   Verdict = "trim"
)

// PR is a pull request as `gh pr list --json` prints it.
type PR struct {
	Number    int       `json:"number"`
	State     string    `json:"state"` // OPEN, CLOSED or MERGED
	HeadName  string    `json:"headRefName"`
	HeadOID   string    `json:"headRefOid"`
	URL       string    `json:"url"`
	MergedAt  time.Time `json:"mergedAt"`
	CrossRepo bool      `json:"isCrossRepository"`
}

const prFields = "number,state,headRefName,headRefOid,url,mergedAt,isCrossRepository"

// Missing older PRs must keep their worktrees.
const defaultPRLimit = 1000

// Worktree is one linked worktree and what Scan decided about it.
type Worktree struct {
	Repo    string // the repository's main checkout
	Path    string
	Branch  string // empty when HEAD is detached
	Head    string
	Batch   string // owning batch, if any
	PR      *PR    // the pull request the verdict rests on, if any
	Verdict Verdict
	Reason  string
}

// Options controls worktree and cache retention.
type Options struct {
	Grace   time.Duration // minimum time since merge
	GH      []string      // command and leading arguments; empty uses gh on PATH
	PRLimit int           // per query; 0 means 1000

	Idle      time.Duration                    // minimum cache inactivity
	MinIdle   time.Duration                    // minimum inactivity even below FreeBelow
	FreeBelow int64                            // target free bytes; 0 disables extra removals
	SlotCap   int64                            // 0 disables the size cap
	Drive     func(path string) (Drive, error) // nil queries the disk
}

// Scan judges linked worktrees in repositories at paths or up to two levels below them.
func Scan(ctx context.Context, paths []string, opt Options) ([]Worktree, error) {
	repos, err := findRepos(ctx, paths)
	if err != nil {
		return nil, err
	}
	owners, ownersErr := batchOwners()
	cwd, _ := os.Getwd()
	var all []Worktree
	for _, repo := range repos {
		wts, err := scanRepo(ctx, repo, owners, ownersErr, cwd, opt)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", repo, err)
		}
		all = append(all, wts...)
	}
	keepHolders(all, repos)
	return all, nil
}

// Apply rechecks and removes approved worktrees, inner ones first to protect retained children.
// Branches are deleted only if they still point at the proven commit.
func Apply(ctx context.Context, wts []Worktree) (removed []Worktree, errs []error) {
	var todo []Worktree
	var stays []string // everything that is not going, including removals that failed
	for _, w := range wts {
		if w.Verdict == Remove {
			todo = append(todo, w)
		} else {
			stays = append(stays, w.Path)
		}
		stays = append(stays, w.Repo)
	}
	sort.SliceStable(todo, func(i, j int) bool { return len(todo[i].Path) > len(todo[j].Path) })
	for _, w := range todo {
		if held := holding(w.Path, stays); held != "" {
			errs = append(errs, fmt.Errorf("%s: kept, it holds %s, which stays", w.Path, held))
			stays = append(stays, w.Path)
			continue
		}
		if why := recheck(ctx, w); why != "" {
			errs = append(errs, fmt.Errorf("%s: kept, %s", w.Path, why))
			stays = append(stays, w.Path)
			continue
		}
		gone, err := removeWorktree(ctx, w)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", w.Path, err))
		}
		if !gone {
			stays = append(stays, w.Path)
			continue
		}
		removed = append(removed, w)
		if w.Branch == "" {
			continue
		}
		if err := deleteBranch(ctx, w.Repo, w.Branch, w.Head); err != nil {
			errs = append(errs, fmt.Errorf("%s: worktree removed, branch %s kept: %w", w.Path, w.Branch, err))
		}
	}
	return removed, errs
}

var removeGitWorktree = func(ctx context.Context, w Worktree) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := gitretry.Output(ctx, func() ([]byte, error) {
		cmd := gitCommand(ctx, w.Repo, "worktree", "remove", w.Path)
		// gitretry matches Git's English error message.
		cmd.Env = append(cmd.Env, "LC_ALL=C", "LANGUAGE=")
		return cmd.CombinedOutput()
	})
	if err != nil {
		return fmt.Errorf("git worktree remove: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

var removeMovedWorktree = os.RemoveAll

var renameWorktree = os.Rename

func removeWorktree(ctx context.Context, w Worktree) (bool, error) {
	err := removeGitWorktree(ctx, w)
	if err == nil {
		return true, nil
	}
	entries, listErr := listWorktrees(ctx, w.Repo)
	if listErr != nil {
		return false, errors.Join(err, listErr)
	}
	if !listed(w.Path, entryPaths(entries)) {
		// Windows handles can prevent final removal after deregistration; only empty or absent folders permit branch cleanup.
		files, readErr := os.ReadDir(w.Path)
		if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
			return false, errors.Join(err, readErr)
		}
		if len(files) != 0 {
			return false, errors.Join(err, batch.RemoveEmptyDir(w.Path))
		}
		if _, pruneErr := gitOut(ctx, w.Repo, "worktree", "prune"); pruneErr != nil {
			return false, errors.Join(err, pruneErr)
		}
		return true, batch.RemoveEmptyDir(w.Path)
	}
	if !strings.Contains(err.Error(), "working trees containing submodules cannot be moved or removed") {
		return false, err
	}
	// foreach skips deinitialized submodules; keep their unchecked repositories and work.
	status, subErr := gitStdout(ctx, w.Path, "submodule", "status", "--recursive")
	if subErr != nil {
		return false, errors.Join(err, subErr)
	}
	for _, line := range strings.Split(status, "\n") {
		if strings.HasPrefix(line, "-") {
			return false, fmt.Errorf("submodules are not initialized: %w", err)
		}
	}
	// Parent status hides submodule work: prove HEADs, files, stashes and local history safe before deleting repositories.
	out, subErr := gitStdout(ctx, w.Path, "submodule", "foreach", "--quiet", "--recursive",
		`test "$(git rev-parse HEAD)" = "$sha1" &&
changes=$(git status --porcelain --untracked-files=all --ignored &&
git stash list &&
git log --oneline HEAD --branches --not --remotes) && test -z "$changes" &&
dir=$(git rev-parse --absolute-git-dir) && printf '%s\0' "$dir"`)
	if subErr != nil {
		return false, errors.Join(err, subErr)
	}
	if subErr := checkSubmoduleRepositories(ctx, w.Path, out); subErr != nil {
		return false, errors.Join(err, subErr)
	}
	// Another process can change the parent while its submodules are checked.
	if why := recheck(ctx, w); why != "" {
		return false, fmt.Errorf("kept, %s", why)
	}
	// Move and deregister before deletion so partial failure cannot leave a registered worktree missing files.
	moved := movedCachePath(w.Path)
	if err := renameWorktree(w.Path, moved); err != nil {
		return false, fmt.Errorf("kept, it could not be moved aside: %s", firstLine(err.Error()))
	}
	// Prune may skip new locks or fail after deregistration; trust the listing when deciding whether to restore.
	_, pruneErr := gitOut(ctx, w.Repo, "worktree", "prune")
	entries, listErr = listWorktrees(ctx, w.Repo)
	if listErr != nil || listed(w.Path, entryPaths(entries)) {
		keptErr := errors.Join(pruneErr, listErr)
		if keptErr == nil {
			keptErr = errors.New("still registered after git worktree prune")
		}
		if err := renameWorktree(moved, w.Path); err != nil {
			return false, fmt.Errorf("moved to %s, which could not be moved back: %w", moved, errors.Join(keptErr, err))
		}
		return false, fmt.Errorf("kept, %w", keptErr)
	}
	if err := removeMovedWorktree(moved); err != nil {
		return true, fmt.Errorf("worktree removed, but %s (moved aside) remains: %w", moved, errors.Join(pruneErr, err))
	}
	return true, pruneErr
}

// Removed submodules can leave private history in gitdir that status and foreach cannot see.
func checkSubmoduleRepositories(ctx context.Context, wt, checked string) error {
	modules, err := gitOut(ctx, wt, "rev-parse", "--git-path", "modules")
	if err != nil {
		return err
	}
	if !filepath.IsAbs(modules) {
		modules = filepath.Join(wt, modules)
	}
	indexed := make(map[string]bool)
	for _, dir := range strings.Split(checked, "\x00") {
		if dir != "" {
			indexed[samePath(dir)] = true
		}
	}
	var walk func(string) error
	walk = func(root string) error {
		return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) && path == root {
				return nil
			}
			if err != nil {
				return err
			}
			if !d.IsDir() && !d.Type().IsRegular() {
				return fmt.Errorf("submodule repository contains a link: %s", path)
			}
			if d.IsDir() && isBare(path) {
				if !indexed[samePath(path)] {
					return fmt.Errorf("submodule repository is not indexed: %s", path)
				}
				// Its linked worktrees live elsewhere and would lose their repository.
				linked, err := os.ReadDir(filepath.Join(path, "worktrees"))
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
				if len(linked) != 0 {
					return fmt.Errorf("submodule repository has linked worktrees: %s", path)
				}
				// Skip the object store, but not repositories nested below it.
				if err := walk(filepath.Join(path, "modules")); err != nil {
					return err
				}
				return filepath.SkipDir
			}
			return nil
		})
	}
	return walk(modules)
}

// Atomic compare-and-delete preserves a branch moved since merge proof.
func deleteBranch(ctx context.Context, repo, branch, oid string) error {
	_, err := gitOut(ctx, repo, "update-ref", "-d", "refs/heads/"+branch, oid)
	return err
}

// Recheck concurrent changes, including ignored files that git worktree remove would delete.
func recheck(ctx context.Context, w Worktree) string {
	entries, err := listWorktrees(ctx, w.Repo)
	if err != nil {
		return "the worktrees could not be listed again: " + firstLine(err.Error())
	}
	for _, e := range entries {
		if samePath(e.path) != samePath(w.Path) {
			continue
		}
		switch {
		case e.locked:
			return "locked since the scan"
		case e.head != w.Head || e.branch != w.Branch:
			return "its HEAD moved since the scan"
		}
		// Approved inner worktrees are gone by now; remaining children have no removal approval.
		if held := holding(w.Path, entryPaths(entries)); held != "" {
			return "it holds the worktree " + held + ", which nothing judged"
		}
		walk := &worktreeWalk{wt: w.Path, registered: entryPaths(entries)}
		if why := unsaved(ctx, walk); why != "" {
			return why + " since the scan"
		}
		if why := walk.linkRisk(); why != "" {
			return why + " since the scan"
		}
		return ""
	}
	return "git no longer lists it as a worktree"
}

// Total sums worktree sizes without double-counting nested worktrees.
func Total(wts []Worktree, sizes map[string]int64) int64 {
	paths := make([]string, len(wts))
	for i, w := range wts {
		paths[i] = w.Path
	}
	var n int64
	for _, w := range wts {
		if !nested(w.Path, paths) {
			n += sizes[w.Path]
		}
	}
	return n
}

func nested(p string, paths []string) bool {
	for _, q := range paths {
		if samePath(q) != samePath(p) && inside(p, q) {
			return true
		}
	}
	return false
}

// Size adds up the regular files under dir, without following links.
func Size(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			n += info.Size()
		}
		return nil
	})
	return n
}

func findRepos(ctx context.Context, paths []string) ([]string, error) {
	seen := map[string]bool{}
	var repos []string
	add := func(dir string) {
		if k := samePath(dir); !seen[k] {
			seen[k] = true
			repos = append(repos, dir)
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
		if main, ok := mainCheckout(ctx, abs); ok {
			add(main)
			continue
		}
		for _, dir := range repoDirs(abs, 2) {
			add(dir)
		}
	}
	return repos, nil
}

func mainCheckout(ctx context.Context, dir string) (string, bool) {
	common, err := gitOut(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", false
	}
	common = filepath.Clean(filepath.FromSlash(common))
	if filepath.Base(common) != ".git" {
		return "", false // a bare repository has no checkout to sweep
	}
	return filepath.Dir(common), true
}

func repoDirs(dir string, depth int) []string {
	if depth == 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var found []string
	for _, e := range entries {
		if !e.IsDir() || e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		sub := filepath.Join(dir, e.Name())
		if info, err := os.Stat(filepath.Join(sub, ".git")); err == nil && info.IsDir() {
			found = append(found, sub)
			continue
		}
		found = append(found, repoDirs(sub, depth-1)...)
	}
	return found
}

type owner struct {
	id      string
	running bool
}

// Unreadable batch records may hide a running owner; refuse rather than guess.
func batchOwners() (map[string]owner, error) {
	results, unreadable, err := batch.List()
	owners := map[string]owner{}
	if err != nil {
		return owners, fmt.Errorf("batch records could not be read: %w", err)
	}
	if unreadable > 0 {
		return owners, fmt.Errorf("%d batch records under %s could not be read; fix or remove them", unreadable, runlog.Home())
	}
	for _, res := range results {
		running := false
		for _, t := range res.Tasks {
			running = running || t.Status == batch.StatusRunning
		}
		for _, t := range res.Tasks {
			if k := samePath(t.Worktree); t.Worktree != "" && owners[k].id == "" {
				owners[k] = owner{id: res.ID, running: running}
			}
		}
	}
	return owners, nil
}

type entry struct {
	path, head, branch     string
	detached, locked, gone bool
}

// NUL-delimited porcelain prevents path contents being mistaken for fields.
func listWorktrees(ctx context.Context, repo string) ([]entry, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var stdout []byte
	out, err := gitretry.Output(ctx, func() ([]byte, error) {
		cmd := gitCommand(ctx, repo, "worktree", "list", "--porcelain", "-z")
		// gitretry matches Git's English error message.
		cmd.Env = append(cmd.Env, "LC_ALL=C", "LANGUAGE=")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		result, runErr := cmd.Output()
		stdout = result
		return stderr.Bytes(), runErr
	})
	if err != nil {
		return nil, fmt.Errorf("git worktree list: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return parseWorktrees(string(stdout)), nil
}

func parseWorktrees(out string) []entry {
	var list []entry
	for _, block := range strings.Split(out, "\x00\x00") {
		var e entry
		for _, line := range strings.Split(block, "\x00") {
			key, val, _ := strings.Cut(line, " ")
			switch key {
			case "worktree":
				e.path = filepath.Clean(filepath.FromSlash(val))
			case "HEAD":
				e.head = val
			case "branch":
				e.branch = strings.TrimPrefix(val, "refs/heads/")
			case "detached":
				e.detached = true
			case "locked":
				e.locked = true
			case "prunable":
				e.gone = true
			}
		}
		if e.path != "" {
			list = append(list, e)
		}
	}
	return list
}

func entryPaths(entries []entry) []string {
	paths := make([]string, len(entries))
	for i, e := range entries {
		paths[i] = e.path
	}
	return paths
}

type repoFacts struct {
	repo      string
	worktrees []string
	prLimit   int
	ownersErr error
	fetchErr  error
	def       string // in full, like refs/remotes/origin/main
	gh        []string
	prs       []PR
	ghErr     error
	ghDone    bool
	landed    map[string][]patchID // landedPatchIDs by merge base, for the run
}

func (r *repoFacts) pullRequests(ctx context.Context) ([]PR, error) {
	if !r.ghDone {
		r.ghDone = true
		r.prs, r.ghErr = listPRs(ctx, r.repo, r.gh, r.prLimit)
	}
	return r.prs, r.ghErr
}

func scanRepo(ctx context.Context, repo string, owners map[string]owner, ownersErr error, cwd string, opt Options) ([]Worktree, error) {
	entries, err := listWorktrees(ctx, repo)
	if err != nil {
		return nil, err
	}
	if len(entries) < 2 {
		return nil, nil // the first entry is the main checkout
	}
	facts := &repoFacts{repo: repo, gh: opt.GH, prLimit: opt.PRLimit, worktrees: entryPaths(entries), ownersErr: ownersErr}
	facts.def, facts.fetchErr = originDefault(ctx, repo)

	var wts []Worktree
	for _, e := range entries[1:] {
		w := Worktree{Repo: repo, Path: e.path, Branch: e.branch, Head: e.head}
		if o, ok := owners[samePath(e.path)]; ok {
			w.Batch = o.id
		}
		w.Verdict, w.Reason = decide(ctx, &w, e, owners, facts, cwd, opt.Grace)
		wts = append(wts, w)
	}
	return wts, nil
}

func decide(ctx context.Context, w *Worktree, e entry, owners map[string]owner, facts *repoFacts, cwd string, grace time.Duration) (Verdict, string) {
	switch {
	case e.gone:
		return Keep, "the folder is gone; git worktree prune clears the entry"
	case e.locked:
		return Keep, "locked"
	case cwd != "" && inside(cwd, w.Path):
		return Keep, "the current directory is inside it"
	}
	walk := &worktreeWalk{wt: w.Path, registered: facts.worktrees}
	if why := unsaved(ctx, walk); why != "" {
		return Keep, why
	}
	if facts.ownersErr != nil {
		return Keep, firstLine(facts.ownersErr.Error())
	}

	if o, ok := owners[samePath(w.Path)]; ok {
		switch {
		case o.running:
			return Keep, "batch " + o.id + " is still running"
		case facts.fetchErr != nil:
			return Keep, "fetch failed: " + firstLine(facts.fetchErr.Error())
		case !isAncestor(ctx, facts.repo, w.Head, facts.def):
			if landed := landedBatchChange(ctx, facts, w.Head); landed != "" {
				if why := walk.linkRisk(); why != "" {
					return Keep, why
				}
				return Remove, fmt.Sprintf("batch %s: its change landed on %s as %s", o.id, strings.TrimPrefix(facts.def, "refs/remotes/"), landed[:7])
			}
			if mergeChangesNothing(ctx, facts, w.Head) {
				if why := walk.linkRisk(); why != "" {
					return Keep, why
				}
				return Remove, fmt.Sprintf("batch %s: merging it into %s changes nothing", o.id, strings.TrimPrefix(facts.def, "refs/remotes/"))
			}
			return Keep, fmt.Sprintf("batch %s: its commit is not on %s", o.id, strings.TrimPrefix(facts.def, "refs/remotes/"))
		}
		if why := walk.linkRisk(); why != "" {
			return Keep, why
		}
		return Remove, fmt.Sprintf("batch %s finished and its commit is on %s", o.id, strings.TrimPrefix(facts.def, "refs/remotes/"))
	}

	if e.detached || w.Branch == "" {
		return Keep, "detached HEAD: no pull request to prove a merge"
	}
	prs, err := facts.pullRequests(ctx)
	if err != nil {
		return Keep, "GitHub unavailable: " + firstLine(err.Error())
	}
	var mine []PR
	for _, p := range prs {
		if p.HeadName == w.Branch && !p.CrossRepo {
			mine = append(mine, p)
		}
	}
	for _, p := range mine {
		if p.State == "OPEN" {
			w.PR = &p
			return Keep, fmt.Sprintf("PR #%d is open", p.Number)
		}
	}
	var lastMerged *PR
	for i := range mine {
		p := mine[i]
		if p.State != "MERGED" {
			continue
		}
		lastMerged = &mine[i]
		if p.HeadOID != w.Head && !isAncestor(ctx, facts.repo, w.Head, p.HeadOID) {
			continue
		}
		w.PR = &mine[i]
		if age := time.Since(p.MergedAt); age < grace {
			return Keep, fmt.Sprintf("PR #%d merged %s ago, inside the %s grace period", p.Number, short(age), short(grace))
		}
		if why := walk.linkRisk(); why != "" {
			return Keep, why
		}
		return Remove, fmt.Sprintf("PR #%d merged %s", p.Number, p.MergedAt.Local().Format("2006-01-02 15:04"))
	}
	if lastMerged != nil {
		w.PR = lastMerged
		return Keep, fmt.Sprintf("commits after merged PR #%d", lastMerged.Number)
	}
	for _, p := range mine {
		w.PR = &p
		return Keep, fmt.Sprintf("PR #%d was closed without merging", p.Number)
	}
	return Keep, "no pull request for " + w.Branch
}

// Other ignored paths may hold irreplaceable work; git worktree remove would delete them too.
var buildOutput = map[string]bool{
	"target": true, "node_modules": true, "dist": true, "build": true, "out": true,
	".next": true, ".nuxt": true, ".turbo": true, ".parcel-cache": true, ".svelte-kit": true,
	".venv": true, "venv": true, "__pycache__": true, ".pytest_cache": true, ".ruff_cache": true,
	".mypy_cache": true, ".tox": true, ".gradle": true, "bin": true, "obj": true, "coverage": true,
	"test-results": true, "playwright-report": true,
}

// Names apply only to folders (status suffix /), never same-named files.
// Tracked folders and tagged fixtures may contain private ignored work; require no tracked files.
func buildFolder(dir, path string, tracksNothing func(prefix string) bool) (string, bool) {
	parts := strings.Split(strings.TrimSuffix(path, "/"), "/")
	isFolder := strings.HasSuffix(path, "/")
	for i, name := range parts {
		if i == len(parts)-1 && !isFolder {
			break
		}
		prefix := strings.Join(parts[:i+1], "/")
		if (buildOutput[name] || isCacheDir(filepath.Join(dir, filepath.FromSlash(prefix)))) && tracksNothing(prefix) {
			return prefix, true
		}
	}
	if !isFolder && strings.HasSuffix(parts[len(parts)-1], ".tsbuildinfo") {
		return "", true
	}
	return "", false
}

func unsaved(ctx context.Context, walk *worktreeWalk) string {
	dir := walk.wt
	// "matching" exposes ignored contents; -z preserves paths and explicit untracked mode overrides user config.
	out, err := gitStdout(ctx, dir, "status", "--porcelain", "-z", "--untracked-files=all", "--ignored=matching")
	if err != nil {
		return "git status failed: " + firstLine(err.Error())
	}
	changed := 0
	var kept []string
	hrn := false
	walked := map[string]bool{} // build folders already handed to walk
	untracked := map[string]bool{}
	tracksNothing := func(prefix string) bool {
		if v, ok := untracked[prefix]; ok {
			return v
		}
		files, err := gitStdout(ctx, dir, "ls-files", "-z", "--", prefix)
		untracked[prefix] = err == nil && files == ""
		return untracked[prefix]
	}
	records := strings.Split(out, "\x00")
	for i := 0; i < len(records); i++ {
		rec := records[i]
		if len(rec) < 4 {
			continue
		}
		code, path := rec[:2], rec[3:]
		if code != "!!" {
			changed++
			if code[0] == 'R' || code[0] == 'C' {
				i++ // a rename or copy is followed by the path it came from
			}
			continue
		}
		if path == ".hrn/" || strings.HasPrefix(path, ".hrn/") {
			hrn = true
			continue
		}
		folder, ok := buildFolder(dir, path, tracksNothing)
		switch {
		case !ok:
			kept = append(kept, strings.TrimSuffix(path, "/"))
		case folder != "" && !walked[folder]:
			walked[folder] = true
			walk.folders = append(walk.folders, folder)
		}
	}
	if why := walk.buildRisk(); why != "" {
		return why
	}
	if hrn {
		kept = append(kept, hrnLeftovers(dir, walk.registered)...)
	}
	if changed == 0 {
		if n, err := hiddenFromStatus(ctx, dir); err != nil {
			return "git ls-files failed: " + firstLine(err.Error())
		} else if n > 0 {
			return fmt.Sprintf("%d files marked assume-unchanged or skip-worktree, whose edits git status does not show", n)
		}
	}
	switch {
	case changed == 1:
		return "1 uncommitted file"
	case changed > 1:
		return fmt.Sprintf("%d uncommitted files", changed)
	case len(kept) > 0:
		if len(kept) > 3 {
			kept = append(kept[:3], fmt.Sprintf("and %d more", len(kept)-3))
		}
		return "ignored files that are not build output: " + strings.Join(kept, ", ")
	}
	return ""
}

// assume-unchanged (lower-case tag) and skip-worktree (S) hide edits from status and worktree remove.
func hiddenFromStatus(ctx context.Context, dir string) (int, error) {
	out, err := gitStdout(ctx, dir, "ls-files", "-v", "-z")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "" {
			continue
		}
		if tag := rec[0]; tag >= 'a' && tag <= 'z' || tag == 'S' {
			n++
		}
	}
	return n, nil
}

// Only registered children get independent verdicts; unregistered task folders may hold private work.
func hrnLeftovers(dir string, registered []string) []string {
	root := filepath.Join(dir, ".hrn")
	entries, err := os.ReadDir(root)
	if err != nil {
		return []string{".hrn (unreadable)"}
	}
	var out []string
	for _, e := range entries {
		switch {
		case e.Name() == ".gitignore" && e.Type().IsRegular():
		case e.Name() == "worktrees" && e.IsDir():
			tasks, err := os.ReadDir(filepath.Join(root, "worktrees"))
			if err != nil {
				out = append(out, ".hrn/worktrees (unreadable)")
			}
			for _, t := range tasks {
				p := filepath.Join(root, "worktrees", t.Name())
				if !t.IsDir() || !listed(p, registered) {
					out = append(out, ".hrn/worktrees/"+t.Name())
				}
			}
		default:
			out = append(out, ".hrn/"+e.Name())
		}
	}
	return out
}

func listed(p string, paths []string) bool {
	for _, q := range paths {
		if samePath(q) == samePath(p) {
			return true
		}
	}
	return false
}

// Whole-folder deletion must preserve nested repositories and refuse unreadable paths.
// Windows git follows node_modules junctions into other checkouts; reject outward or unreadable links.
func buildRisk(wt, folder string) string {
	rel := func(p string) string {
		r, _ := filepath.Rel(wt, p)
		return filepath.ToSlash(r)
	}
	kind, at, err := walkRisk(wt, folder)
	switch {
	case kind == riskRepo:
		return "a repository inside build output: " + rel(at)
	case kind == riskLink:
		return "a link out of the worktree inside build output: " + rel(at)
	case err != nil:
		return "build output that cannot be read: " + rel(folder)
	}
	return ""
}

var treeWalks atomic.Int64

type worktreeWalk struct {
	wt         string
	registered []string // worktrees git lists; inner ones have their own verdicts
	folders    []string // build folders, slash-separated, in git status order

	done  bool
	risks map[string]string // why a build folder cannot go, "" when it can
	link  string            // why the rest of the worktree cannot go
}

func (w *worktreeWalk) buildRisk() string {
	for _, folder := range w.folders {
		if why := linkAbove(w.wt, folder); why != "" {
			return why
		}
		w.run()
		why, ok := w.risks[folder]
		if !ok { // reached only through a link that stays inside, so walk it there
			why = buildRisk(w.wt, filepath.Join(w.wt, filepath.FromSlash(folder)))
		}
		if why != "" {
			return why
		}
	}
	return ""
}

// Windows git deletes through directory links even in tracked folders; other platforms unlink them.
func (w *worktreeWalk) linkRisk() string {
	w.run()
	return w.link
}

func (w *worktreeWalk) run() {
	if w.done {
		return
	}
	w.done = true
	treeWalks.Add(1)
	w.risks = make(map[string]string, len(w.folders))
	root := samePath(w.wt)
	listed := make(map[string]bool, len(w.registered))
	for _, reg := range w.registered {
		listed[samePath(reg)] = true
	}
	build := make(map[string]bool, len(w.folders))
	above := map[string]bool{} // folders on the way to a build folder
	for _, f := range w.folders {
		build[f] = true
		for d := path.Dir(f); d != "."; d = path.Dir(d) {
			above[d] = true
		}
	}
	windows := runtime.GOOS == "windows"
	skip := func(d fs.DirEntry) error {
		if d != nil && d.IsDir() {
			return fs.SkipDir
		}
		return nil
	}
	cur := ""              // current build folder
	left := len(w.folders) // unreached build folders
	_ = filepath.WalkDir(w.wt, func(p string, d fs.DirEntry, err error) error {
		if p == w.wt {
			if err != nil {
				w.link = "the worktree cannot be read: " + firstLine(err.Error())
				return err
			}
			return nil
		}
		r, rerr := filepath.Rel(w.wt, p)
		if rerr != nil {
			return rerr
		}
		rel := filepath.ToSlash(r)
		if cur != "" && rel != cur && !strings.HasPrefix(rel, cur+"/") {
			cur = ""
		}
		if cur == "" && build[rel] {
			cur = rel
			w.risks[cur] = ""
			left--
		}
		if cur != "" {
			if w.risks[cur] != "" {
				return skip(d)
			}
			if err != nil {
				w.risks[cur] = "build output that cannot be read: " + cur
				return skip(d)
			}
			switch kind, at := riskAt(w.wt, p, d); kind {
			case riskRepo:
				w.risks[cur] = "a repository inside build output: " + relSlash(w.wt, at)
			case riskLink:
				w.risks[cur] = "a link out of the worktree inside build output: " + relSlash(w.wt, at)
			}
			return nil
		}
		if err != nil {
			if w.link == "" {
				w.link = "the worktree cannot be read: " + firstLine(err.Error())
			}
			return skip(d)
		}
		if d.IsDir() {
			// WalkDir only adds names read under wt, so no directory needs EvalSymlinks.
			key := filepath.Join(root, r)
			if windows {
				key = strings.ToLower(key)
			}
			if listed[key] || d.Name() == ".git" || !windows && !above[rel] {
				return fs.SkipDir
			}
			return nil
		}
		if windows && w.link == "" && !d.Type().IsRegular() && deletesThrough(w.wt, p) {
			w.link = "a link out of the worktree: " + rel
		}
		if w.link != "" && left == 0 {
			return fs.SkipAll
		}
		return nil
	})
}

func relSlash(wt, p string) string {
	r, _ := filepath.Rel(wt, p)
	return filepath.ToSlash(r)
}

// Unreadable targets must count as unsafe directory links on Windows.
func deletesThrough(wt, p string) bool {
	to, err := os.Readlink(p)
	if err != nil {
		return true
	}
	if inside(linkTarget(p, to), wt) {
		return false
	}
	info, err := os.Stat(p)
	return err != nil || info.IsDir()
}

const (
	riskRepo = "repository"
	riskLink = "link"
)

func walkRisk(root, folder string) (kind, at string, err error) {
	err = filepath.WalkDir(folder, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if kind, at = riskAt(root, p, d); kind != "" {
			return fs.SkipAll
		}
		return nil
	})
	if kind != "" {
		err = nil
	}
	return kind, at, err
}

func riskAt(root, p string, d fs.DirEntry) (kind, at string) {
	switch {
	case !d.IsDir() && !d.Type().IsRegular(): // a symlink, or a junction (irregular)
		if to, err := os.Readlink(p); err != nil || !inside(linkTarget(p, to), root) {
			return riskLink, p
		}
		if d.Name() == ".git" {
			return riskRepo, filepath.Dir(p)
		}
	case d.Name() == ".git":
		return riskRepo, filepath.Dir(p)
	case d.IsDir() && d.Name() == "objects" && isBare(filepath.Dir(p)):
		return riskRepo, filepath.Dir(p)
	}
	return "", ""
}

// Check ancestors too: Windows git follows junctions above build output into other checkouts.
func linkAbove(wt, folder string) string {
	p := wt
	for _, part := range strings.Split(folder, "/") {
		p = filepath.Join(p, part)
		info, err := os.Lstat(p)
		if err != nil {
			return "a folder above build output cannot be read: " + part
		}
		if info.IsDir() || info.Mode().IsRegular() {
			continue
		}
		if to, err := os.Readlink(p); err != nil || !inside(linkTarget(p, to), wt) {
			r, _ := filepath.Rel(wt, p)
			return "a link out of the worktree above build output: " + filepath.ToSlash(r)
		}
	}
	return ""
}

// Bare repositories have no .git; HEAD may symlink to an unborn ref via core.preferSymlinkRefs.
func isBare(dir string) bool {
	head, err1 := os.Lstat(filepath.Join(dir, "HEAD"))
	refs, err2 := os.Lstat(filepath.Join(dir, "refs"))
	return err1 == nil && err2 == nil && refs.IsDir() &&
		(head.Mode().IsRegular() || head.Mode()&fs.ModeSymlink != 0)
}

func linkTarget(link, to string) string {
	if filepath.IsAbs(to) {
		return to
	}
	return filepath.Join(filepath.Dir(link), to)
}

// Cache Directory Tagging requires this signature at the start of CACHEDIR.TAG.
const cacheTagSignature = "Signature: 8a477f597d28d172789f06886806bc55"

// A filename alone is not permission to delete; require the tag's signature.
func isCacheDir(dir string) bool {
	tag := filepath.Join(dir, "CACHEDIR.TAG")
	if info, err := os.Lstat(tag); err != nil || !info.Mode().IsRegular() {
		return false
	}
	f, err := os.Open(tag)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, len(cacheTagSignature))
	_, err = io.ReadFull(f, head)
	return err == nil && string(head) == cacheTagSignature
}

// A removal deletes any repository or retained worktree inside it; git ignores .hrn/ and will not stop it.
func keepHolders(all []Worktree, repos []string) {
	for changed := true; changed; {
		changed = false
		for i := range all {
			if all[i].Verdict != Remove {
				continue
			}
			if held := holding(all[i].Path, repos); held != "" {
				all[i].Verdict, all[i].Reason = Keep, "holds the repository "+held
				changed = true
				continue
			}
			for j := range all {
				if j != i && all[j].Verdict != Remove && inside(all[j].Path, all[i].Path) {
					all[i].Verdict, all[i].Reason = Keep, "holds a worktree that stays: "+all[j].Path
					changed = true
					break
				}
			}
		}
	}
}

// Ask origin and fetch by name to avoid stale origin/HEAD and narrow refspecs.
// Return the full tracking ref: a local branch or tag can shadow origin/main.
func originDefault(ctx context.Context, repo string) (string, error) {
	out, err := gitOut(ctx, repo, "ls-remote", "--symref", "origin", "HEAD")
	if err != nil {
		return "", err
	}
	var name string
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
			name, _, _ = strings.Cut(rest, "\t")
			break
		}
	}
	if name == "" {
		return "", errors.New("origin does not say which branch is its default")
	}
	if _, err := gitOut(ctx, repo, "fetch", "--quiet", "origin", "+refs/heads/"+name+":refs/remotes/origin/"+name); err != nil {
		return "", err
	}
	return "refs/remotes/origin/" + name, nil
}

// Missing commits cannot prove ancestry.
func isAncestor(ctx context.Context, repo, a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	_, err := gitOut(ctx, repo, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

var maxCommits = 2000

// Require the whole task diff to match one landed commit; incomplete comparisons prove nothing.
func landedBatchChange(ctx context.Context, facts *repoFacts, head string) string {
	base, err := gitOut(ctx, facts.repo, "merge-base", head, facts.def)
	if err != nil || base == "" {
		return ""
	}
	landed, ok := facts.landed[base]
	if !ok {
		landed = landedPatchIDs(ctx, facts.repo, base, facts.def)
		if facts.landed == nil {
			facts.landed = map[string][]patchID{}
		}
		facts.landed[base] = landed
	}
	if len(landed) == 0 {
		return ""
	}
	taskDiff, err := gitStdout(ctx, facts.repo, "diff", "--no-ext-diff", "--no-textconv", "--submodule=short", "--binary", base, head)
	if err != nil || taskDiff == "" {
		return ""
	}
	taskID, err := gitStdoutInput(ctx, facts.repo, taskDiff, "patch-id", "--verbatim")
	if err != nil {
		return ""
	}
	taskFields := strings.Fields(taskID)
	if len(taskFields) != 2 {
		return ""
	}
	for _, p := range landed {
		if p.id == taskFields[0] {
			return p.commit
		}
	}
	return ""
}

// A change split over several landed commits, or inside a larger one, leaves the merge nothing to do.
func mergeChangesNothing(ctx context.Context, facts *repoFacts, head string) bool {
	base, err := gitOut(ctx, facts.repo, "merge-base", head, facts.def)
	if err != nil || base == "" {
		return false
	}
	trees, err := gitOut(ctx, facts.repo, "rev-parse", base+"^{tree}", head+"^{tree}", facts.def+"^{tree}")
	tree := strings.Fields(trees)
	// A task that changed nothing proves nothing.
	if err != nil || len(tree) != 3 || tree[0] == tree[1] {
		return false
	}
	// A rename moves a content merge to a path that only the other side changed.
	var paths string
	for _, tip := range []string{head, facts.def} {
		changed, err := gitStdout(ctx, facts.repo, "diff", "--no-renames", "--name-only", "-z", base, tip)
		if err != nil {
			return false
		}
		paths += changed
	}
	// A union or custom merge driver can keep the branch's side and hide the task's change;
	// both commands read attributes from the branch, so this sees what the merge uses.
	source := "--attr-source=" + facts.def
	attrs, err := gitStdoutInput(ctx, facts.repo, paths, source, "check-attr", "-z", "--stdin", "merge")
	fields := strings.Split(attrs, "\x00")
	if err != nil || len(fields)%3 != 1 {
		return false
	}
	for i := 2; i < len(fields); i += 3 {
		if fields[i] != "unspecified" {
			return false
		}
	}
	// merge.default would pick the driver for files with no merge attribute, a configured one first.
	if v, err := gitOut(ctx, facts.repo, "config", "--default=", "--get", "merge.default"); err != nil || v != "" {
		return false
	}
	merged, err := gitStdout(ctx, facts.repo, source, "merge-tree", "--write-tree", "--no-messages", facts.def, head)
	return err == nil && strings.TrimSpace(merged) == tree[2]
}

type patchID struct{ id, commit string }

var landedPatchIDs = func(ctx context.Context, repo, base, def string) []patchID {
	countText, err := gitOut(ctx, repo, "rev-list", "--count", "--max-count="+strconv.Itoa(maxCommits), "--no-merges", base+".."+def)
	if err != nil {
		return nil
	}
	count, err := strconv.Atoi(countText)
	if err != nil || count == 0 || count >= maxCommits {
		return nil
	}
	out, err := logPatchIDs(ctx, repo, base, def)
	if err != nil {
		return nil
	}
	var ids []patchID
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[1]) < 7 {
			return nil
		}
		ids = append(ids, patchID{id: fields[0], commit: fields[1]})
	}
	return ids
}

// Stream patches so large binary history is not held in memory.
func logPatchIDs(ctx context.Context, repo, base, def string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	log := gitCommand(ctx, repo, "log", "-p", "--no-ext-diff", "--no-textconv", "--submodule=short", "--no-merges", "--binary", "--format=commit %H", "--max-count="+strconv.Itoa(maxCommits), base+".."+def)
	// A descendant can retain stderr after git exits on Windows.
	log.WaitDelay = time.Second
	var logErr bytes.Buffer
	log.Stderr = &logErr
	patches, err := log.StdoutPipe()
	if err != nil {
		return "", err
	}
	ids := gitCommand(ctx, repo, "patch-id", "--verbatim")
	ids.Stdin = patches
	var out, idsErr bytes.Buffer
	ids.Stdout, ids.Stderr = &out, &idsErr
	if err := log.Start(); err != nil {
		return "", err
	}
	if err := ids.Run(); err != nil {
		// A git log blocked on a full pipe would otherwise never exit.
		cancel()
		_ = log.Wait()
		return "", fmt.Errorf("git patch-id: %v: %s", err, strings.TrimSpace(idsErr.String()))
	}
	if err := log.Wait(); err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return "", fmt.Errorf("git log: %v: %s", err, strings.TrimSpace(logErr.String()))
	}
	return out.String(), nil
}

// Query open PRs separately: a truncated all-state list could hide one and falsely permit deletion.
// A full open list proves nothing and must fail.
func listPRs(ctx context.Context, repo string, gh []string, limit int) ([]PR, error) {
	url, err := gitOut(ctx, repo, "remote", "get-url", "origin")
	if err != nil {
		return nil, errors.New("the repository has no origin remote")
	}
	if len(gh) == 0 {
		gh = []string{"gh"}
	}
	if limit <= 0 {
		limit = defaultPRLimit
	}
	open, err := ghPRs(ctx, repo, gh, url, "open", limit)
	if err != nil {
		return nil, err
	}
	if len(open) >= limit {
		return nil, fmt.Errorf("%d open pull requests or more, so gh's list may be cut short", limit)
	}
	all, err := ghPRs(ctx, repo, gh, url, "all", limit)
	if err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	for _, p := range open {
		seen[p.Number] = true
	}
	for _, p := range all {
		if !seen[p.Number] {
			open = append(open, p)
		}
	}
	return open, nil
}

func ghPRs(ctx context.Context, repo string, gh []string, url, state string, limit int) ([]PR, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	args := append(append([]string{}, gh[1:]...), "pr", "list", "-R", url, "--state", state, "--limit", strconv.Itoa(limit), "--json", prFields)
	cmd := exec.CommandContext(ctx, gh[0], args...)
	cmd.Dir = repo
	cmd.Env = cleanEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(msg)
	}
	var prs []PR
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("reading gh's answer: %w", err)
	}
	return prs, nil
}

// Inherited repository variables, including those from hooks, must not override the chosen path.
var repoVars = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
	"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE",
}

// Inherited git -c settings such as url.*.insteadOf could redirect merge proof to another repository.
func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(name)
		drop := upper == "GIT_CONFIG_PARAMETERS" || upper == "GIT_CONFIG_COUNT" ||
			strings.HasPrefix(upper, "GIT_CONFIG_KEY_") || strings.HasPrefix(upper, "GIT_CONFIG_VALUE_")
		for _, v := range repoVars {
			drop = drop || upper == v
		}
		if !drop {
			env = append(env, kv)
		}
	}
	// Replace refs can fake ancestry; require original history for merge proof.
	return append(env, "GIT_NO_REPLACE_OBJECTS=1")
}

func gitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = cleanEnv()
	return cmd
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := gitCommand(ctx, dir, args...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), text)
	}
	return text, nil
}

func holding(dir string, paths []string) string {
	for _, p := range paths {
		if samePath(p) != samePath(dir) && inside(p, dir) {
			return p
		}
	}
	return ""
}

// Preserve -z data: trimming or mixing stderr would corrupt paths and records.
func gitStdout(ctx context.Context, dir string, args ...string) (string, error) {
	return gitStdoutInput(ctx, dir, "", args...)
}

func gitStdoutInput(ctx context.Context, dir, input string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := gitCommand(ctx, dir, args...)
	cmd.Stdin = strings.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		text := strings.TrimSpace(stderr.String())
		if text == "" {
			text = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), text)
	}
	return string(out), nil
}

func inside(p, dir string) bool {
	p, dir = samePath(p), samePath(dir)
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, string(filepath.Separator))+string(filepath.Separator))
}

// Resolve Windows 8.3 aliases and case differences before containment comparisons.
func samePath(p string) string {
	p = filepath.Clean(filepath.FromSlash(p))
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

func short(d time.Duration) string {
	h, m := int(d.Hours()), int(d.Minutes())%60
	switch {
	case h == 0:
		return fmt.Sprintf("%dm", m)
	case m == 0:
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}
