package gc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// TestHelperGH stands in for `gh pr list`: it answers from the JSON file
// HRN_GH_HELPER names as gh does, newest first, filtered by --state and cut at
// --limit, or fails as gh does when it is not logged in.
func TestHelperGH(t *testing.T) {
	v := os.Getenv("HRN_GH_HELPER")
	switch v {
	case "":
		return
	case "fail":
		fmt.Fprintln(os.Stderr, "To get started with GitHub CLI, please run:  gh auth login")
		os.Exit(4)
	}
	data, err := os.ReadFile(v)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var prs []PR
	if err := json.Unmarshal(data, &prs); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	state, limit := "open", 30 // gh's own defaults
	args := os.Args
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--state":
			state = args[i+1]
		case "--limit":
			fmt.Sscan(args[i+1], &limit)
		}
	}
	sort.Slice(prs, func(i, j int) bool { return prs[i].Number > prs[j].Number })
	var out []PR
	for _, p := range prs {
		if (state == "all" || strings.EqualFold(p.State, state)) && len(out) < limit {
			out = append(out, p)
		}
	}
	json.NewEncoder(os.Stdout).Encode(out)
	os.Exit(0)
}

var fakeGH = []string{os.Args[0], "-test.run=^TestHelperGH$", "--"}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// fixture is a repository with an origin, its worktrees beside it, and a
// fake GitHub answering with prs.
type fixture struct {
	t      *testing.T
	root   string // holds origin.git, repo and the worktrees
	origin string
	repo   string
	prs    []PR
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	t.Setenv("HRN_HOME", t.TempDir())
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, root: root, origin: filepath.Join(root, "origin.git"), repo: filepath.Join(root, "repo")}
	git(t, root, "init", "-q", "--bare", "-b", "main", f.origin)
	git(t, root, "init", "-q", "-b", "main", f.repo)
	git(t, f.repo, "config", "user.email", "test@example.com")
	git(t, f.repo, "config", "user.name", "Test")
	f.write(f.repo, ".gitignore", ".hrn/\ntarget/\n")
	f.write(f.repo, "README.md", "hello\n")
	git(t, f.repo, "add", ".")
	git(t, f.repo, "commit", "-q", "-m", "init")
	git(t, f.repo, "remote", "add", "origin", f.origin)
	git(t, f.repo, "push", "-q", "origin", "main")
	git(t, f.repo, "remote", "set-head", "origin", "main")
	f.serve()
	return f
}

func (f *fixture) write(dir, name, body string) {
	f.t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// worktree adds a worktree on a new branch from main, at root/name unless
// name is already absolute, with one commit of its own.
func (f *fixture) worktree(name, branch string) (path, head string) {
	f.t.Helper()
	path = name
	if !filepath.IsAbs(path) {
		path = filepath.Join(f.root, name)
	}
	git(f.t, f.repo, "worktree", "add", "-q", "-b", branch, path, "main")
	f.write(path, branch+".txt", branch+"\n")
	git(f.t, path, "add", ".")
	git(f.t, path, "commit", "-q", "-m", branch)
	return path, git(f.t, path, "rev-parse", "HEAD")
}

// land merges a branch into main and pushes main, as a merged pull request does.
func (f *fixture) land(branch string) {
	f.t.Helper()
	git(f.t, f.repo, "merge", "-q", "--no-ff", "-m", "Merge "+branch, branch)
	git(f.t, f.repo, "push", "-q", "origin", "main")
}

func (f *fixture) pr(n int, branch, state, head string, mergedAgo time.Duration) {
	p := PR{Number: n, State: state, HeadName: branch, HeadOID: head, URL: fmt.Sprintf("https://github.com/o/r/pull/%d", n)}
	if state == "MERGED" {
		p.MergedAt = time.Now().Add(-mergedAgo).UTC()
	}
	f.prs = append(f.prs, p)
	f.serve()
}

// serve writes the fake GitHub's answer and points the helper at it.
func (f *fixture) serve() {
	f.t.Helper()
	data, err := json.Marshal(f.prs)
	if err != nil {
		f.t.Fatal(err)
	}
	file := filepath.Join(f.root, "prs.json")
	if err := os.WriteFile(file, data, 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.t.Setenv("HRN_GH_HELPER", file)
}

func (f *fixture) scan() []Worktree {
	f.t.Helper()
	return f.scanWith(Options{Grace: time.Hour, GH: fakeGH})
}

func (f *fixture) scanWith(opt Options) []Worktree {
	f.t.Helper()
	wts, err := Scan(context.Background(), []string{f.repo}, opt)
	if err != nil {
		f.t.Fatal(err)
	}
	return wts
}

func find(t *testing.T, wts []Worktree, path string) Worktree {
	t.Helper()
	for _, w := range wts {
		if samePath(w.Path) == samePath(path) {
			return w
		}
	}
	t.Fatalf("%s is not in the scan: %+v", path, wts)
	return Worktree{}
}

func wantVerdict(t *testing.T, w Worktree, v Verdict, reason string) {
	t.Helper()
	if w.Verdict != v || !strings.Contains(w.Reason, reason) {
		t.Errorf("%s: got %s (%s), want %s with a reason containing %q", filepath.Base(w.Path), w.Verdict, w.Reason, v, reason)
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestGCRemovesAWorktreeWhosePullRequestMergedAtItsHead(t *testing.T) {
	f := newFixture(t)
	merged, head := f.worktree("wt-merged", "feature/done")
	f.land("feature/done")
	f.pr(7, "feature/done", "MERGED", head, 2*time.Hour)
	open, openHead := f.worktree("wt-open", "feature/going")
	f.pr(8, "feature/going", "OPEN", openHead, 0)

	wts := f.scan()
	w := find(t, wts, merged)
	wantVerdict(t, w, Remove, "PR #7 merged")
	if w.PR == nil || w.PR.URL != "https://github.com/o/r/pull/7" {
		t.Errorf("the verdict should carry the pull request, got %+v", w.PR)
	}
	wantVerdict(t, find(t, wts, open), Keep, "PR #8 is open")

	removed, errs := Apply(context.Background(), wts)
	if len(errs) > 0 || len(removed) != 1 {
		t.Fatalf("apply: removed %d, errors %v", len(removed), errs)
	}
	if exists(merged) {
		t.Error("the merged worktree is still on disk")
	}
	if out := git(t, f.repo, "branch", "--list", "feature/done"); out != "" {
		t.Errorf("the merged branch was not deleted: %q", out)
	}
	if !exists(open) || git(t, f.repo, "branch", "--list", "feature/going") == "" {
		t.Error("the worktree with an open pull request, or its branch, was removed")
	}
}

func TestGCKeepsWorktreesWithoutAPullRequestMergedAtTheirHead(t *testing.T) {
	f := newFixture(t)
	closed, closedHead := f.worktree("wt-closed", "closed")
	f.pr(1, "closed", "CLOSED", closedHead, 0)
	none, _ := f.worktree("wt-none", "no-pr")
	after, afterHead := f.worktree("wt-after", "after")
	f.pr(2, "after", "MERGED", afterHead, 2*time.Hour)
	f.write(after, "more.txt", "a commit after the merge\n")
	git(t, after, "add", ".")
	git(t, after, "commit", "-q", "-m", "after the merge")
	elsewhere, _ := f.worktree("wt-elsewhere", "elsewhere")
	f.pr(3, "elsewhere", "MERGED", strings.Repeat("a", 40), 2*time.Hour)
	reopened, reopenedHead := f.worktree("wt-reopened", "reopened")
	f.pr(4, "reopened", "MERGED", reopenedHead, 3*time.Hour)
	f.pr(5, "reopened", "OPEN", reopenedHead, 0)

	wts := f.scan()
	wantVerdict(t, find(t, wts, closed), Keep, "PR #1 was closed without merging")
	wantVerdict(t, find(t, wts, none), Keep, "no pull request")
	wantVerdict(t, find(t, wts, after), Keep, "commits after merged PR #2")
	wantVerdict(t, find(t, wts, elsewhere), Keep, "commits after merged PR #3")
	wantVerdict(t, find(t, wts, reopened), Keep, "PR #5 is open")
}

func TestGCFindsAnOpenPullRequestTheFullListCutOff(t *testing.T) {
	f := newFixture(t)
	wt, head := f.worktree("wt", "b")
	f.pr(1, "b", "OPEN", head, 0)
	f.pr(2, "other", "MERGED", strings.Repeat("b", 40), 2*time.Hour)
	f.pr(3, "b", "MERGED", head, 2*time.Hour)

	// Two pull requests at a time: the full list stops at #3 and #2.
	wantVerdict(t, find(t, f.scanWith(Options{Grace: time.Hour, GH: fakeGH, PRLimit: 2}), wt), Keep, "PR #1 is open")
}

func TestGCKeepsBranchWorktreesWhenTheOpenListMayBeCutShort(t *testing.T) {
	f := newFixture(t)
	wt, head := f.worktree("wt", "b")
	f.pr(1, "x", "OPEN", strings.Repeat("a", 40), 0)
	f.pr(2, "y", "OPEN", strings.Repeat("c", 40), 0)
	f.pr(3, "b", "MERGED", head, 2*time.Hour)

	wantVerdict(t, find(t, f.scanWith(Options{Grace: time.Hour, GH: fakeGH, PRLimit: 2}), wt), Keep, "GitHub unavailable")
}

func TestGCRemovesAWorktreeWhoseHeadIsInsideTheMergedHead(t *testing.T) {
	// The branch moved on elsewhere (pushed from another clone) and merged
	// there: everything this worktree holds is in the merged head.
	f := newFixture(t)
	behind, head := f.worktree("wt-behind", "moved-on")
	later := git(t, f.repo, "commit-tree", head+"^{tree}", "-p", head, "-m", "pushed from elsewhere")
	f.pr(9, "moved-on", "MERGED", later, 2*time.Hour)

	wantVerdict(t, find(t, f.scan(), behind), Remove, "PR #9 merged")
}

func TestGCKeepsAWorktreeMergedWithinTheGracePeriod(t *testing.T) {
	f := newFixture(t)
	fresh, head := f.worktree("wt-fresh", "fresh")
	f.pr(4, "fresh", "MERGED", head, 10*time.Minute)

	wantVerdict(t, find(t, f.scan(), fresh), Keep, "merged 10m ago")
}

func TestGCKeepsDirtyLockedAndDetachedWorktrees(t *testing.T) {
	f := newFixture(t)
	modified, h1 := f.worktree("wt-modified", "modified")
	f.write(modified, "README.md", "changed\n")
	untracked, h2 := f.worktree("wt-untracked", "untracked")
	f.write(untracked, "notes.txt", "not added\n")
	locked, h3 := f.worktree("wt-locked", "locked")
	git(t, f.repo, "worktree", "lock", locked)
	for i, b := range []string{"modified", "untracked", "locked"} {
		f.pr(10+i, b, "MERGED", []string{h1, h2, h3}[i], 2*time.Hour)
	}
	detached := filepath.Join(f.root, "wt-detached")
	git(t, f.repo, "worktree", "add", "-q", "--detach", detached, "main")

	wts := f.scan()
	wantVerdict(t, find(t, wts, modified), Keep, "1 uncommitted file")
	wantVerdict(t, find(t, wts, untracked), Keep, "1 uncommitted file")
	wantVerdict(t, find(t, wts, locked), Keep, "locked")
	wantVerdict(t, find(t, wts, detached), Keep, "detached HEAD")
}

func TestGCKeepsAWorktreeHoldingIgnoredFilesThatAreNotBuildOutput(t *testing.T) {
	f := newFixture(t)
	f.write(f.repo, ".gitignore", ".hrn/\ntarget/\n.env\n*.tsbuildinfo\ntest-results/\n")
	git(t, f.repo, "commit", "-q", "-am", "ignore .env")
	git(t, f.repo, "push", "-q", "origin", "main")
	secret, h1 := f.worktree("wt-env", "env")
	f.write(secret, ".env", "LOCAL=1\n")
	built, h2 := f.worktree("wt-built", "built")
	f.write(built, filepath.Join("target", "debug", "app"), "binary\n")
	f.write(built, filepath.Join("apps", "web", "tsconfig.tsbuildinfo"), "{}\n")
	f.write(built, filepath.Join("apps", "web", "test-results", "trace.zip"), "zip\n")
	f.pr(1, "env", "MERGED", h1, 2*time.Hour)
	f.pr(2, "built", "MERGED", h2, 2*time.Hour)

	wts := f.scan()
	wantVerdict(t, find(t, wts, secret), Keep, "ignored files that are not build output: .env")
	wantVerdict(t, find(t, wts, built), Remove, "PR #2 merged")
}

func TestGCKeepsAWorktreeWhoseHrnFolderHoldsMoreThanWorktrees(t *testing.T) {
	f := newFixture(t)
	notes, h1 := f.worktree("wt-notes", "notes")
	f.write(notes, filepath.Join(".hrn", "notes.txt"), "mine\n")
	leftover, h2 := f.worktree("wt-leftover", "leftover")
	f.write(leftover, filepath.Join(".hrn", "worktrees", "old-task", "half.txt"), "a task git no longer lists\n")
	f.pr(1, "notes", "MERGED", h1, 2*time.Hour)
	f.pr(2, "leftover", "MERGED", h2, 2*time.Hour)

	wts := f.scan()
	wantVerdict(t, find(t, wts, notes), Keep, ".hrn/notes.txt")
	wantVerdict(t, find(t, wts, leftover), Keep, ".hrn/worktrees/old-task")
}

func TestGCKeepsAWorktreeHoldingARepository(t *testing.T) {
	f := newFixture(t)
	wt, head := f.worktree("wt", "done")
	f.pr(1, "done", "MERGED", head, 2*time.Hour)
	clone := filepath.Join(wt, "target", "clone")
	git(t, f.root, "init", "-q", "-b", "main", clone)

	wts, err := Scan(context.Background(), []string{f.repo, clone}, Options{Grace: time.Hour, GH: fakeGH})
	if err != nil {
		t.Fatal(err)
	}
	wantVerdict(t, find(t, wts, wt), Keep, "repository")
}

func TestGCSeesUntrackedFilesWhateverStatusShows(t *testing.T) {
	f := newFixture(t)
	wt, head := f.worktree("wt", "done")
	f.pr(1, "done", "MERGED", head, 2*time.Hour)
	git(t, f.repo, "config", "status.showUntrackedFiles", "no")
	f.write(wt, "notes.txt", "not added\n")

	wantVerdict(t, find(t, f.scan(), wt), Keep, "1 uncommitted file")
}

func TestGCCountsAStagedRenameAsOneChange(t *testing.T) {
	f := newFixture(t)
	wt, head := f.worktree("wt", "done")
	f.pr(1, "done", "MERGED", head, 2*time.Hour)
	git(t, wt, "mv", "done.txt", "renamed.txt")

	wantVerdict(t, find(t, f.scan(), wt), Keep, "1 uncommitted file")
}

func TestGCReadsPathsGitWouldQuote(t *testing.T) {
	f := newFixture(t)
	f.write(f.repo, ".gitignore", ".hrn/\ntarget/\ncafé-cache/\n")
	git(t, f.repo, "commit", "-q", "-am", "ignore the cache")
	wt, head := f.worktree("wt", "done")
	f.write(wt, filepath.Join("café-cache", "CACHEDIR.TAG"), "Signature: 8a477f597d28d172789f06886806bc55\n")
	f.pr(1, "done", "MERGED", head, 2*time.Hour)

	wantVerdict(t, find(t, f.scan(), wt), Remove, "merged")
}

func TestGCTakesOnlyFoldersByTheirBuildNames(t *testing.T) {
	f := newFixture(t)
	f.write(f.repo, ".gitignore", ".hrn/\ntarget/\nbuild\n")
	git(t, f.repo, "commit", "-q", "-am", "ignore build")
	wt, head := f.worktree("wt", "done")
	f.write(wt, "build", "#!/bin/sh\n# a script of mine, not a build folder\n")
	f.pr(1, "done", "MERGED", head, 2*time.Hour)

	wantVerdict(t, find(t, f.scan(), wt), Keep, "not build output: build")
}

func TestGCFindsARepositoryInsideBuildOutputOnItsOwn(t *testing.T) {
	f := newFixture(t)
	wt, head := f.worktree("wt", "done")
	f.pr(1, "done", "MERGED", head, 2*time.Hour)
	git(t, f.root, "init", "-q", "-b", "main", filepath.Join(wt, "target", "deps", "clone"))

	wantVerdict(t, find(t, f.scan(), wt), Keep, "a repository inside build output: target/deps/clone")
}

func TestGCKeepsAWorktreeThatHoldsOneThatStays(t *testing.T) {
	f := newFixture(t)
	outer, head := f.worktree("wt-outer", "outer")
	f.pr(1, "outer", "MERGED", head, 2*time.Hour)
	inner, innerHead := f.worktree(filepath.Join(outer, ".hrn", "worktrees", "task"), "hrn/task")
	f.pr(2, "hrn/task", "OPEN", innerHead, 0)

	wts := f.scan()
	wantVerdict(t, find(t, wts, outer), Keep, "holds a worktree that stays")
	wantVerdict(t, find(t, wts, inner), Keep, "PR #2 is open")
}

func TestGCRemovesNestedMergedWorktreesInnerFirst(t *testing.T) {
	f := newFixture(t)
	outer, head := f.worktree("wt-outer", "outer")
	f.pr(1, "outer", "MERGED", head, 2*time.Hour)
	inner, innerHead := f.worktree(filepath.Join(outer, ".hrn", "worktrees", "task"), "inner")
	f.pr(2, "inner", "MERGED", innerHead, 2*time.Hour)

	wts := f.scan()
	wantVerdict(t, find(t, wts, outer), Remove, "merged")
	wantVerdict(t, find(t, wts, inner), Remove, "merged")
	removed, errs := Apply(context.Background(), wts)
	if len(errs) > 0 || len(removed) != 2 || exists(outer) {
		t.Fatalf("removed %d, errors %v, outer still there: %v", len(removed), errs, exists(outer))
	}
	if out := git(t, f.repo, "worktree", "list", "--porcelain"); strings.Count(out, "worktree ") != 1 {
		t.Errorf("git still lists removed worktrees:\n%s", out)
	}
}

// saveBatch writes a batch record the way hrn batch leaves one.
func saveBatch(t *testing.T, id, repo string, tasks ...batch.TaskResult) {
	t.Helper()
	data, err := json.Marshal(batch.Result{ID: id, Started: time.Now(), Dir: repo, Tasks: tasks})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(runlog.Home(), "batches")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGCRemovesAFinishedBatchWorktreeOnlyWhenItsCommitIsOnTheDefaultBranch(t *testing.T) {
	f := newFixture(t)
	landed, _ := f.worktree(filepath.Join(f.repo, ".hrn", "worktrees", "landed"), "hrn/landed")
	f.land("hrn/landed")
	unlanded, _ := f.worktree(filepath.Join(f.repo, ".hrn", "worktrees", "unlanded"), "hrn/unlanded")
	saveBatch(t, "20261011-100000-aaaaaa", f.repo,
		batch.TaskResult{ID: "landed", Status: batch.StatusPass, Branch: "hrn/landed", Worktree: landed},
		batch.TaskResult{ID: "unlanded", Status: batch.StatusPass, Branch: "hrn/unlanded", Worktree: unlanded})
	running, _ := f.worktree(filepath.Join(f.repo, ".hrn", "worktrees", "running"), "hrn/running")
	f.land("hrn/running")
	saveBatch(t, "20261011-110000-bbbbbb", f.repo,
		batch.TaskResult{ID: "running", Status: batch.StatusRunning, Branch: "hrn/running", Worktree: running})

	wts := f.scan()
	wantVerdict(t, find(t, wts, landed), Remove, "batch 20261011-100000-aaaaaa")
	wantVerdict(t, find(t, wts, unlanded), Keep, "not on origin/main")
	wantVerdict(t, find(t, wts, running), Keep, "still running")
}

func TestGCKeepsEveryWorktreeWhileABatchRecordCannotBeRead(t *testing.T) {
	f := newFixture(t)
	wt, head := f.worktree("wt", "done")
	f.pr(1, "done", "MERGED", head, 2*time.Hour)
	saveBatch(t, "20261011-100000-aaaaaa", f.repo) // a readable one beside it
	f.write(filepath.Join(runlog.Home(), "batches"), "20261011-120000-cccccc.json", "{ half a record")

	wantVerdict(t, find(t, f.scan(), wt), Keep, "could not be read")
}

func TestGCJudgesBatchWorktreesAgainstTheBranchOriginNamesNow(t *testing.T) {
	f := newFixture(t)
	git(t, f.repo, "push", "-q", "origin", "main:develop")
	git(t, f.origin, "symbolic-ref", "HEAD", "refs/heads/develop")
	// This clone still believes origin's default is main.
	landed, _ := f.worktree(filepath.Join(f.repo, ".hrn", "worktrees", "landed"), "hrn/landed")
	f.land("hrn/landed")
	saveBatch(t, "20261011-100000-aaaaaa", f.repo,
		batch.TaskResult{ID: "landed", Status: batch.StatusPass, Branch: "hrn/landed", Worktree: landed})

	wantVerdict(t, find(t, f.scan(), landed), Keep, "not on origin/develop")
}

func TestGCIsNotFooledByALocalBranchNamedLikeOrigins(t *testing.T) {
	f := newFixture(t)
	task, head := f.worktree(filepath.Join(f.repo, ".hrn", "worktrees", "task"), "hrn/task")
	saveBatch(t, "20261011-100000-aaaaaa", f.repo,
		batch.TaskResult{ID: "task", Status: batch.StatusPass, Branch: "hrn/task", Worktree: task})
	git(t, f.repo, "branch", "origin/main", head) // refs/heads/origin/main, which git reads before refs/remotes

	wantVerdict(t, find(t, f.scan(), task), Keep, "not on origin/main")
}

func TestGCFailsClosedWhenGitHubOrTheFetchIsUnavailable(t *testing.T) {
	f := newFixture(t)
	merged, head := f.worktree("wt-merged", "done")
	f.land("done")
	f.pr(1, "done", "MERGED", head, 2*time.Hour)
	landed, _ := f.worktree(filepath.Join(f.repo, ".hrn", "worktrees", "landed"), "hrn/landed")
	f.land("hrn/landed")
	saveBatch(t, "20261011-100000-aaaaaa", f.repo,
		batch.TaskResult{ID: "landed", Status: batch.StatusPass, Branch: "hrn/landed", Worktree: landed})

	t.Setenv("HRN_GH_HELPER", "fail")
	wantVerdict(t, find(t, f.scan(), merged), Keep, "GitHub unavailable")

	f.serve()
	git(t, f.repo, "remote", "set-url", "origin", filepath.Join(f.root, "gone.git"))
	wts := f.scan()
	wantVerdict(t, find(t, wts, landed), Keep, "fetch failed")
	// GitHub's word on the exact head needs no fetch.
	wantVerdict(t, find(t, wts, merged), Remove, "PR #1 merged")
}

func TestGCKeepsTheWorktreeItRunsIn(t *testing.T) {
	f := newFixture(t)
	wt, head := f.worktree("wt", "done")
	f.pr(1, "done", "MERGED", head, 2*time.Hour)
	t.Chdir(wt)

	wantVerdict(t, find(t, f.scan(), wt), Keep, "the current directory is inside it")
}

func TestGCTakesNoProofFromAForkWithTheSameBranchName(t *testing.T) {
	f := newFixture(t)
	wt, head := f.worktree("wt", "patch-1")
	f.prs = append(f.prs, PR{Number: 1, State: "MERGED", HeadName: "patch-1", HeadOID: head,
		URL: "https://github.com/o/r/pull/1", MergedAt: time.Now().Add(-2 * time.Hour), CrossRepo: true})
	f.serve()

	wantVerdict(t, find(t, f.scan(), wt), Keep, "no pull request")
}

func TestPathsCompareAlikeHoweverWindowsSpellsThem(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("case and separators only differ on Windows")
	}
	if !inside(`C:/Work/Repo/.hrn/worktrees/t`, `c:\work\repo`) {
		t.Error("a forward-slash, mixed-case path under the folder was not seen inside it")
	}
	if inside(`C:\Work\Repo-2`, `C:\Work\Repo`) {
		t.Error("a sibling sharing the folder's name as a prefix was seen inside it")
	}
	if samePath(`C:\Work\Repo\`) != samePath(`c:/work/repo`) {
		t.Error("the same folder spelt two ways compared different")
	}
}

func TestGCScansTheRepositoryItIsGivenWhateverGitDirSays(t *testing.T) {
	f := newFixture(t)
	mine, head := f.worktree("wt-mine", "done")
	f.pr(1, "done", "MERGED", head, 2*time.Hour)
	other := filepath.Join(f.root, "other")
	git(t, f.root, "init", "-q", "-b", "main", other)
	git(t, other, "-c", "user.email=t@example.com", "-c", "user.name=T", "commit", "-q", "--allow-empty", "-m", "init")
	theirs := filepath.Join(f.root, "wt-theirs")
	git(t, other, "worktree", "add", "-q", "-b", "x", theirs)
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)

	wts := f.scan()
	if len(wts) != 1 || samePath(wts[0].Path) != samePath(mine) {
		t.Fatalf("scanning %s found %+v; want only its own worktree", f.repo, wts)
	}
}

func TestGCNeverListsTheMainCheckout(t *testing.T) {
	f := newFixture(t)
	f.worktree("wt", "branch")
	for _, w := range f.scan() {
		if samePath(w.Path) == samePath(f.repo) {
			t.Fatalf("the main checkout is in the scan: %+v", w)
		}
	}
}

func TestGCFindsTheRepositoriesInAFolderAndFromInsideAWorktree(t *testing.T) {
	f := newFixture(t)
	wt, head := f.worktree("wt", "done")
	f.pr(1, "done", "MERGED", head, 2*time.Hour)

	for _, from := range []string{f.root, wt} {
		wts, err := Scan(context.Background(), []string{from}, Options{Grace: time.Hour, GH: fakeGH})
		if err != nil {
			t.Fatal(err)
		}
		if len(wts) != 1 || samePath(wts[0].Path) != samePath(wt) {
			t.Errorf("scanning %s found %+v, want the one worktree", from, wts)
		}
	}
}

func TestGCApplyLeavesAWorktreeThatChangedSinceTheScan(t *testing.T) {
	f := newFixture(t)
	wt, head := f.worktree("wt", "done")
	f.pr(1, "done", "MERGED", head, 2*time.Hour)
	wts := f.scan()
	wantVerdict(t, find(t, wts, wt), Remove, "merged")

	f.write(wt, "late.txt", "written after the scan\n")
	removed, errs := Apply(context.Background(), wts)
	if len(removed) != 0 || len(errs) != 1 {
		t.Fatalf("removed %d, errors %v; want git to refuse the changed worktree", len(removed), errs)
	}
	if !exists(filepath.Join(wt, "late.txt")) {
		t.Error("the file written after the scan is gone")
	}
	if git(t, f.repo, "branch", "--list", "done") == "" {
		t.Error("the branch was deleted although its worktree stayed")
	}
}

func TestGCApplyLeavesAWorktreeThatMovedOnSinceTheScan(t *testing.T) {
	f := newFixture(t)
	f.write(f.repo, ".gitignore", ".hrn/\ntarget/\n.env\n")
	git(t, f.repo, "commit", "-q", "-am", "ignore .env")
	committed, h1 := f.worktree("wt-committed", "committed")
	secret, h2 := f.worktree("wt-secret", "secret")
	locked, h3 := f.worktree("wt-locked", "locked")
	f.pr(1, "committed", "MERGED", h1, 2*time.Hour)
	f.pr(2, "secret", "MERGED", h2, 2*time.Hour)
	f.pr(3, "locked", "MERGED", h3, 2*time.Hour)
	wts := f.scan()
	for _, p := range []string{committed, secret, locked} {
		wantVerdict(t, find(t, wts, p), Remove, "merged")
	}

	f.write(committed, "more.txt", "a commit no pull request has\n")
	git(t, committed, "add", ".")
	git(t, committed, "commit", "-q", "-m", "after the scan")
	f.write(secret, ".env", "LOCAL=1\n")
	git(t, f.repo, "worktree", "lock", locked)
	removed, errs := Apply(context.Background(), wts)
	if len(removed) != 0 || len(errs) != 3 {
		t.Fatalf("removed %d, errors %v; want all three to stay", len(removed), errs)
	}
	for _, p := range []string{filepath.Join(committed, "more.txt"), filepath.Join(secret, ".env"), locked} {
		if !exists(p) {
			t.Errorf("%s is gone", p)
		}
	}
}

func TestGCApplyKeepsAParentThatGainedAWorktreeSinceTheScan(t *testing.T) {
	f := newFixture(t)
	outer, head := f.worktree("wt-outer", "outer")
	f.pr(1, "outer", "MERGED", head, 2*time.Hour)
	wts := f.scan()
	wantVerdict(t, find(t, wts, outer), Remove, "merged")

	inner, _ := f.worktree(filepath.Join(outer, ".hrn", "worktrees", "new"), "hrn/new")
	f.write(inner, "work.txt", "an agent's work in progress\n")
	removed, errs := Apply(context.Background(), wts)
	if len(removed) != 0 || len(errs) != 1 {
		t.Fatalf("removed %d, errors %v; want the outer worktree kept", len(removed), errs)
	}
	if !exists(filepath.Join(inner, "work.txt")) {
		t.Error("the new worktree's work is gone")
	}
}

func TestGCApplyKeepsAParentWhenTheWorktreeInsideItCannotGo(t *testing.T) {
	f := newFixture(t)
	outer, head := f.worktree("wt-outer", "outer")
	f.pr(1, "outer", "MERGED", head, 2*time.Hour)
	inner, innerHead := f.worktree(filepath.Join(outer, ".hrn", "worktrees", "task"), "inner")
	f.pr(2, "inner", "MERGED", innerHead, 2*time.Hour)
	wts := f.scan()
	wantVerdict(t, find(t, wts, outer), Remove, "merged")

	f.write(inner, "late.txt", "written after the scan\n")
	removed, errs := Apply(context.Background(), wts)
	if len(removed) != 0 || len(errs) != 2 {
		t.Fatalf("removed %d, errors %v; want both to stay", len(removed), errs)
	}
	if !exists(filepath.Join(inner, "late.txt")) {
		t.Error("removing the outer worktree deleted the inner one's new file")
	}
}

func TestDeleteBranchOnlyWhileItPointsAtTheCheckedCommit(t *testing.T) {
	f := newFixture(t)
	checked := git(t, f.repo, "rev-parse", "HEAD")
	git(t, f.repo, "branch", "moved", checked)
	later := git(t, f.repo, "commit-tree", checked+"^{tree}", "-p", checked, "-m", "moved on")
	git(t, f.repo, "update-ref", "refs/heads/moved", later)

	if err := deleteBranch(context.Background(), f.repo, "moved", checked); err == nil {
		t.Error("a branch that moved on was deleted")
	}
	if git(t, f.repo, "rev-parse", "refs/heads/moved") != later {
		t.Fatal("the branch that moved on changed")
	}
	if err := deleteBranch(context.Background(), f.repo, "moved", later); err != nil {
		t.Fatalf("deleting at its own tip: %v", err)
	}
	if out := git(t, f.repo, "branch", "--list", "moved"); out != "" {
		t.Errorf("the branch is still there: %q", out)
	}
}

func TestTotalCountsAWorktreeInsideAnotherOnce(t *testing.T) {
	root := t.TempDir()
	outer, inner, other := filepath.Join(root, "a"), filepath.Join(root, "a", ".hrn", "worktrees", "t"), filepath.Join(root, "b")
	wts := []Worktree{{Path: outer}, {Path: inner}, {Path: other}}
	sizes := map[string]int64{outer: 10, inner: 4, other: 1} // outer's 10 holds inner's 4
	if got := Total(wts, sizes); got != 11 {
		t.Errorf("Total = %d, want 11", got)
	}
}

func TestSizeAddsUpTheFilesUnderAFolder(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, n := range map[string]int{"x": 100, filepath.Join("a", "y"): 20, filepath.Join("a", "b", "z"): 3} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := Size(dir); got != 123 {
		t.Errorf("Size = %d, want 123", got)
	}
}
