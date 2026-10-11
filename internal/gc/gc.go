// Package gc finds the git worktrees whose work is already merged and removes
// them, with their build output. A worktree goes only on proof: GitHub says
// its pull request merged at the commit it holds, or, for one an hrn batch
// made, that commit is on origin's default branch. Anything it cannot prove
// stays, with the reason, and when GitHub or the fetch cannot be reached the
// answer is to keep.
package gc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
)

type Verdict string

const (
	Remove Verdict = "remove"
	Keep   Verdict = "keep"
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

// prLimit is how many pull requests one repository is read for. A branch
// whose pull request is older than that finds none, and its worktree stays.
const prLimit = "1000"

// Worktree is one linked worktree and what Scan decided about it.
type Worktree struct {
	Repo    string // the repository's main checkout
	Path    string
	Branch  string // empty when HEAD is detached
	Head    string
	Batch   string // the hrn batch that made it, if any
	PR      *PR    // the pull request the verdict rests on, if any
	Verdict Verdict
	Reason  string
}

type Options struct {
	Grace time.Duration // a pull request merged more recently than this keeps its worktree
	GH    []string      // the gh command and leading arguments; empty means gh on PATH
}

// Scan lists the linked worktrees of every repository at paths, each with a
// verdict. A path is a repository (any directory inside it) or a folder whose
// repositories are found up to two levels down. Main checkouts are never listed.
func Scan(ctx context.Context, paths []string, opt Options) ([]Worktree, error) {
	repos, err := findRepos(ctx, paths)
	if err != nil {
		return nil, err
	}
	owners := batchOwners()
	cwd, _ := os.Getwd()
	var all []Worktree
	for _, repo := range repos {
		wts, err := scanRepo(ctx, repo, owners, cwd, opt)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", repo, err)
		}
		all = append(all, wts...)
	}
	keepHolders(all)
	return all, nil
}

// Apply removes the worktrees Scan marked Remove, inner ones first, then each
// one's branch if it still points at the commit Scan checked. Each worktree is
// checked again just before it goes, and one that changed since the scan, or
// holds one that stays, stays too.
func Apply(ctx context.Context, wts []Worktree) (removed []Worktree, errs []error) {
	var todo []Worktree
	var stays []string // everything that is not going, including removals that failed
	for _, w := range wts {
		if w.Verdict == Remove {
			todo = append(todo, w)
		} else {
			stays = append(stays, w.Path)
		}
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
		if _, err := gitOut(ctx, w.Repo, "worktree", "remove", w.Path); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", w.Path, err))
			stays = append(stays, w.Path)
			continue
		}
		removed = append(removed, w)
		if w.Branch == "" {
			continue
		}
		if tip, err := gitOut(ctx, w.Repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+w.Branch); err == nil && tip == w.Head {
			if _, err := gitOut(ctx, w.Repo, "branch", "-D", w.Branch); err != nil {
				errs = append(errs, fmt.Errorf("%s: worktree removed, branch kept: %w", w.Path, err))
			}
		}
	}
	return removed, errs
}

// recheck says why a worktree marked for removal must stay after all, because
// it changed since the scan: locked, a new HEAD or branch, or new files that
// no branch holds, ignored ones included (git worktree remove deletes those).
func recheck(ctx context.Context, w Worktree) string {
	out, err := gitOut(ctx, w.Repo, "worktree", "list", "--porcelain")
	if err != nil {
		return "the worktrees could not be listed again: " + firstLine(err.Error())
	}
	for _, e := range parseWorktrees(out) {
		if samePath(e.path) != samePath(w.Path) {
			continue
		}
		switch {
		case e.locked:
			return "locked since the scan"
		case e.head != w.Head || e.branch != w.Branch:
			return "its HEAD moved since the scan"
		}
		if why := unsaved(ctx, w.Path); why != "" {
			return why + " since the scan"
		}
		return ""
	}
	return "git no longer lists it as a worktree"
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

// findRepos resolves paths to main checkouts, once each.
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

// mainCheckout returns the main checkout of the repository dir is in.
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

// repoDirs finds main checkouts (a .git directory, not a worktree's .git file)
// under dir, depth levels down, without looking inside a repository.
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

// batchOwners maps each worktree an hrn batch record names to its batch,
// the newest record first.
func batchOwners() map[string]owner {
	results, _, err := batch.List()
	owners := map[string]owner{}
	if err != nil {
		return owners
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
	return owners
}

type entry struct {
	path, head, branch     string
	detached, locked, gone bool
}

func parseWorktrees(out string) []entry {
	var list []entry
	for _, block := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n\n") {
		var e entry
		for _, line := range strings.Split(block, "\n") {
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

// repoFacts is what one repository's worktrees are judged against, gathered
// once: the fetch, the default branch, and GitHub's pull requests.
type repoFacts struct {
	repo     string
	fetchErr error
	def      string // like origin/main; empty when origin names none
	gh       []string
	prs      []PR
	ghErr    error
	ghDone   bool
}

func (r *repoFacts) pullRequests(ctx context.Context) ([]PR, error) {
	if !r.ghDone {
		r.ghDone = true
		r.prs, r.ghErr = listPRs(ctx, r.repo, r.gh)
	}
	return r.prs, r.ghErr
}

func scanRepo(ctx context.Context, repo string, owners map[string]owner, cwd string, opt Options) ([]Worktree, error) {
	out, err := gitOut(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	entries := parseWorktrees(out)
	if len(entries) < 2 {
		return nil, nil // the first entry is the main checkout
	}
	facts := &repoFacts{repo: repo, gh: opt.GH}
	if _, err := gitOut(ctx, repo, "fetch", "--quiet", "origin"); err != nil {
		facts.fetchErr = err
	}
	facts.def = defaultBranch(ctx, repo)

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

// decide gives one worktree its verdict: keep, with the first reason that
// holds it, or remove, with the proof that its work is merged.
func decide(ctx context.Context, w *Worktree, e entry, owners map[string]owner, facts *repoFacts, cwd string, grace time.Duration) (Verdict, string) {
	switch {
	case e.gone:
		return Keep, "the folder is gone; git worktree prune clears the entry"
	case e.locked:
		return Keep, "locked"
	case cwd != "" && inside(cwd, w.Path):
		return Keep, "the current directory is inside it"
	}
	if why := unsaved(ctx, w.Path); why != "" {
		return Keep, why
	}

	if o, ok := owners[samePath(w.Path)]; ok {
		switch {
		case o.running:
			return Keep, "batch " + o.id + " is still running"
		case facts.fetchErr != nil:
			return Keep, "fetch failed: " + firstLine(facts.fetchErr.Error())
		case facts.def == "":
			return Keep, "origin names no default branch"
		case !isAncestor(ctx, facts.repo, w.Head, facts.def):
			return Keep, fmt.Sprintf("batch %s: its commit is not on %s", o.id, facts.def)
		}
		return Remove, fmt.Sprintf("batch %s finished and its commit is on %s", o.id, facts.def)
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

// buildOutput names the ignored folders that are only ever generated, which
// go with the worktree. Any other ignored file or folder keeps it, because
// git worktree remove would delete it and no branch holds it.
var buildOutput = map[string]bool{
	"target": true, "node_modules": true, "dist": true, "build": true, "out": true,
	".next": true, ".nuxt": true, ".turbo": true, ".parcel-cache": true, ".svelte-kit": true,
	".venv": true, "venv": true, "__pycache__": true, ".pytest_cache": true, ".ruff_cache": true,
	".mypy_cache": true, ".tox": true, ".gradle": true, "bin": true, "obj": true, "coverage": true,
	"test-results": true, "playwright-report": true, ".hrn": true,
}

// isBuildOutput reports an ignored path gc may delete with its worktree.
func isBuildOutput(dir, path string) bool {
	name := filepath.Base(strings.TrimSuffix(path, "/"))
	return buildOutput[name] || strings.HasSuffix(name, ".tsbuildinfo") || isCacheDir(filepath.Join(dir, path))
}

// unsaved says why a worktree holds something no branch has: tracked changes,
// untracked files, or ignored files that are not build output. Empty means
// nothing.
func unsaved(ctx context.Context, dir string) string {
	// "matching" lists the paths an ignore pattern matched; the default mode
	// shows a folder of nothing but ignored files as the folder, which hides
	// what is in it.
	out, err := gitOut(ctx, dir, "status", "--porcelain", "--ignored=matching")
	if err != nil {
		return "git status failed: " + firstLine(err.Error())
	}
	changed := 0
	var kept []string
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		path, ignored := strings.CutPrefix(line, "!! ")
		if !ignored {
			changed++
			continue
		}
		path = strings.Trim(strings.TrimSpace(path), `"`)
		if isBuildOutput(dir, path) {
			continue
		}
		kept = append(kept, strings.TrimSuffix(path, "/"))
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

// isCacheDir reports a folder tagged as a cache (the Cache Directory Tagging
// convention; Cargo writes the tag into every target folder).
func isCacheDir(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "CACHEDIR.TAG"))
	return err == nil
}

// keepHolders keeps any worktree that holds one that stays: removing it would
// delete the inner one with it (.hrn/ is ignored, so git would not stop it).
func keepHolders(all []Worktree) {
	for changed := true; changed; {
		changed = false
		for i := range all {
			if all[i].Verdict != Remove {
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

func defaultBranch(ctx context.Context, repo string) string {
	if ref, err := gitOut(ctx, repo, "rev-parse", "--abbrev-ref", "origin/HEAD"); err == nil && ref != "origin/HEAD" {
		return ref
	}
	for _, ref := range []string{"origin/main", "origin/master"} {
		if _, err := gitOut(ctx, repo, "rev-parse", "--verify", "--quiet", ref); err == nil {
			return ref
		}
	}
	return ""
}

// isAncestor reports whether commit a is reachable from b. A commit git does
// not have counts as no.
func isAncestor(ctx context.Context, repo, a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	_, err := gitOut(ctx, repo, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

func listPRs(ctx context.Context, repo string, gh []string) ([]PR, error) {
	url, err := gitOut(ctx, repo, "remote", "get-url", "origin")
	if err != nil {
		return nil, errors.New("the repository has no origin remote")
	}
	if len(gh) == 0 {
		gh = []string{"gh"}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	args := append(append([]string{}, gh[1:]...), "pr", "list", "-R", url, "--state", "all", "--limit", prLimit, "--json", prFields)
	cmd := exec.CommandContext(ctx, gh[0], args...)
	cmd.Dir = repo
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

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), text)
	}
	return text, nil
}

// holding returns the first of paths that lies under dir, or "".
func holding(dir string, paths []string) string {
	for _, p := range paths {
		if samePath(p) != samePath(dir) && inside(p, dir) {
			return p
		}
	}
	return ""
}

// inside reports whether p is dir or under it.
func inside(p, dir string) bool {
	p, dir = samePath(p), samePath(dir)
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, string(filepath.Separator))+string(filepath.Separator))
}

// samePath normalises a path for comparison: git prints forward slashes and
// long names on Windows, where paths are also case-insensitive.
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

// short prints a duration in hours and minutes: 10m, 2h, 1h30m.
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
