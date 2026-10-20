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
	"sync"
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
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = cleanEnv()
	out, err := cmd.CombinedOutput()
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

// Each git process can take seconds on a loaded machine, so the origin and repo
// pair is built once per test binary and copied into every test.
var (
	templateOnce sync.Once
	templateDir  string
	templateErr  error
)

func buildTemplate() (string, error) {
	dir, err := os.MkdirTemp("", "hrn-gc-fixture-")
	if err != nil {
		return "", err
	}
	origin, repo := filepath.Join(dir, "origin.git"), filepath.Join(dir, "repo")
	run := func(dir string, args ...string) error {
		// Detached auto maintenance would still touch the template while tests copy it.
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "maintenance.auto=false", "-c", "gc.auto=0"}, args...)...)
		cmd.Env = cleanEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %v\n%s", args, err, out)
		}
		return nil
	}
	steps := [][]string{
		{dir, "init", "-q", "--bare", "-b", "main", origin},
		{dir, "init", "-q", "-b", "main", repo},
		{repo, "config", "user.email", "test@example.com"},
		{repo, "config", "user.name", "Test"},
	}
	for _, s := range steps {
		if err := run(s[0], s[1:]...); err != nil {
			return dir, err
		}
	}
	for name, body := range map[string]string{".gitignore": ".hrn/\ntarget/\n", "README.md": "hello\n"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			return dir, err
		}
	}
	steps = [][]string{
		{repo, "add", "."},
		{repo, "commit", "-q", "-m", "init"},
		{repo, "remote", "add", "origin", origin},
		// The local transport drops -c, so the receiver gets the settings itself.
		{repo, "push", "-q", "--receive-pack=git -c maintenance.auto=false -c gc.auto=0 receive-pack", "origin", "main"},
		{repo, "remote", "set-head", "origin", "main"},
	}
	for _, s := range steps {
		if err := run(s[0], s[1:]...); err != nil {
			return dir, err
		}
	}
	return dir, nil
}

func TestGCFixtureBuildsStartNoAutoMaintenance(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "trace")
	t.Setenv("GIT_TRACE", trace)
	dir, err := buildTemplate()
	defer os.RemoveAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := submoduleFixture(t)
	out, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	// Only a traced receiver proves the push's own maintenance is covered.
	if !strings.Contains(string(out), "built-in: git receive-pack") {
		t.Fatalf("the trace holds no receive-pack, so the receiver went unchecked:\n%s", out)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.Contains(line, "maintenance run") || strings.Contains(line, "gc --auto") {
			t.Errorf("a fixture build started git's auto maintenance: %s", line)
		}
	}
	// The copies must keep git's defaults, so the settings never reach a config file.
	for _, root := range []string{dir, f.root} {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || d.Name() != "config" {
				return err
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.Contains(string(body), "[maintenance]") || strings.Contains(string(body), "[gc]") {
				t.Errorf("%s holds maintenance settings:\n%s", path, body)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	t.Setenv("HRN_HOME", t.TempDir())
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	templateOnce.Do(func() { templateDir, templateErr = buildTemplate() })
	if templateErr != nil {
		t.Fatalf("fixture template: %v", templateErr)
	}
	f := &fixture{t: t, root: root, origin: filepath.Join(root, "origin.git"), repo: filepath.Join(root, "repo")}
	for _, name := range []string{"origin.git", "repo"} {
		if err := os.CopyFS(filepath.Join(root, name), os.DirFS(filepath.Join(templateDir, name))); err != nil {
			t.Fatal(err)
		}
	}
	git(t, f.repo, "remote", "set-url", "origin", f.origin)
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

func TestGCTakesFilesInsideABuildFolderAsBuildOutput(t *testing.T) {
	// pytest ignores its cache with a .gitignore of its own, so git lists the
	// files in it one by one rather than the folder.
	f := newFixture(t)
	wt, head := f.worktree("wt", "done")
	f.write(wt, filepath.Join(".pytest_cache", ".gitignore"), "*\n")
	f.write(wt, filepath.Join(".pytest_cache", "v", "cache", "nodeids"), "[]\n")
	f.write(wt, filepath.Join("src", ".cache-x", "CACHEDIR.TAG"), "Signature: 8a477f597d28d172789f06886806bc55\n")
	f.write(wt, filepath.Join("src", ".cache-x", ".gitignore"), "*\n")
	f.pr(1, "done", "MERGED", head, 2*time.Hour)

	wantVerdict(t, find(t, f.scan(), wt), Remove, "merged")
}

func TestGCTrustsACacheTagOnlyInAnUntrackedFolderWithTheSignature(t *testing.T) {
	f := newFixture(t)
	f.write(f.repo, ".gitignore", ".hrn/\ntarget/\nfixtures/notes.txt\nmydata/\n")
	f.write(f.repo, filepath.Join("fixtures", "CACHEDIR.TAG"), "Signature: 8a477f597d28d172789f06886806bc55\n")
	git(t, f.repo, "add", ".")
	git(t, f.repo, "commit", "-q", "-m", "a test fixture that is a tagged cache")
	tracked, h1 := f.worktree("wt-fixture", "fixture")
	f.write(tracked, filepath.Join("fixtures", "notes.txt"), "mine\n")
	unsigned, h2 := f.worktree("wt-unsigned", "unsigned")
	f.write(unsigned, filepath.Join("mydata", "CACHEDIR.TAG"), "not the signature\n")
	f.write(unsigned, filepath.Join("mydata", "results.csv"), "1,2\n")
	f.pr(1, "fixture", "MERGED", h1, 2*time.Hour)
	f.pr(2, "unsigned", "MERGED", h2, 2*time.Hour)

	wts := f.scan()
	wantVerdict(t, find(t, wts, tracked), Keep, "not build output: fixtures/notes.txt")
	wantVerdict(t, find(t, wts, unsigned), Keep, "not build output: mydata")
}

func TestGCTakesOnlyFoldersByTheirBuildNames(t *testing.T) {
	f := newFixture(t)
	f.write(f.repo, ".gitignore", ".hrn/\ntarget/\nbuild\nbin/local.sh\n")
	f.write(f.repo, filepath.Join("bin", "run.sh"), "#!/bin/sh\n")
	git(t, f.repo, "add", ".")
	git(t, f.repo, "commit", "-q", "-m", "ignore build, track bin")
	wt, head := f.worktree("wt", "done")
	f.write(wt, "build", "#!/bin/sh\n# a script of mine, not a build folder\n")
	tracked, head2 := f.worktree("wt-bin", "bin")
	f.write(tracked, filepath.Join("bin", "local.sh"), "#!/bin/sh\n# mine, beside the tracked run.sh\n")
	f.pr(1, "done", "MERGED", head, 2*time.Hour)
	f.pr(2, "bin", "MERGED", head2, 2*time.Hour)

	wts := f.scan()
	wantVerdict(t, find(t, wts, wt), Keep, "not build output: build")
	wantVerdict(t, find(t, wts, tracked), Keep, "not build output: bin/local.sh")
}

func TestGCFindsARepositoryInsideBuildOutputOnItsOwn(t *testing.T) {
	f := newFixture(t)
	wt, head := f.worktree("wt", "done")
	f.pr(1, "done", "MERGED", head, 2*time.Hour)
	git(t, f.root, "init", "-q", "-b", "main", filepath.Join(wt, "target", "deps", "clone"))

	wantVerdict(t, find(t, f.scan(), wt), Keep, "a repository inside build output: target/deps/clone")
}

// link makes a directory link the way the platform's tools do: a junction on
// Windows (npm and pnpm make these in node_modules), a symlink elsewhere.
func link(t *testing.T, at, to string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", at, to).CombinedOutput(); err != nil {
			t.Fatalf("mklink /J: %v\n%s", err, out)
		}
		return
	}
	if err := os.Symlink(to, at); err != nil {
		t.Fatal(err)
	}
}

func TestGCKeepsAWorktreeWhoseBuildOutputLinksOutOfIt(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Join(f.root, "store")
	f.write(outside, "package.json", "{}\n")
	wt, head := f.worktree("wt", "done")
	link(t, filepath.Join(wt, "target", "dep"), outside)
	inner, head2 := f.worktree("wt-inner", "inner")
	link(t, filepath.Join(inner, "target", "self"), filepath.Join(inner, "src"))
	f.pr(1, "done", "MERGED", head, 2*time.Hour)
	f.pr(2, "inner", "MERGED", head2, 2*time.Hour)

	before := treeWalks.Load()
	wts := f.scan()
	if n := treeWalks.Load() - before; n != 2 {
		t.Errorf("the scan walked the worktrees %d times, want once each (2)", n)
	}
	wantVerdict(t, find(t, wts, wt), Keep, "a link out of the worktree inside build output: target/dep")
	wantVerdict(t, find(t, wts, inner), Remove, "merged") // a link to its own folder goes with it

	before = treeWalks.Load()
	Apply(context.Background(), wts)
	if n := treeWalks.Load() - before; n != 1 {
		t.Errorf("the recheck walked %d times, want 1", n)
	}
}

func TestGCFindsABareRepositoryInsideBuildOutput(t *testing.T) {
	f := newFixture(t)
	wt, head := f.worktree("wt", "done")
	f.pr(1, "done", "MERGED", head, 2*time.Hour)
	git(t, f.root, "init", "-q", "--bare", filepath.Join(wt, "target", "backup.git"))

	wantVerdict(t, find(t, f.scan(), wt), Keep, "a repository inside build output: target/backup.git")
}

func TestGCKeepsAWorktreeWithFilesGitIsToldNotToLookAt(t *testing.T) {
	f := newFixture(t)
	wt, head := f.worktree("wt", "done")
	f.pr(1, "done", "MERGED", head, 2*time.Hour)
	git(t, wt, "update-index", "--assume-unchanged", "README.md")
	f.write(wt, "README.md", "local edits git status will not show\n")

	wantVerdict(t, find(t, f.scan(), wt), Keep, "assume-unchanged")
}

func TestGCIgnoresReplaceRefsWhenItChecksAncestry(t *testing.T) {
	f := newFixture(t)
	task, taskHead := f.worktree(filepath.Join(f.repo, ".hrn", "worktrees", "task"), "hrn/task")
	saveBatch(t, "20261011-100000-aaaaaa", f.repo,
		batch.TaskResult{ID: "task", Status: batch.StatusPass, Branch: "hrn/task", Worktree: task})
	f.worktree("wt-other", "other")
	f.land("other")
	tip := git(t, f.repo, "rev-parse", "main")
	git(t, f.repo, "replace", "--graft", tip, taskHead) // pretend main's tip descends from the task

	wantVerdict(t, find(t, f.scan(), task), Keep, "not on origin/main")
}

func TestGCFindsABareRepositoryWhoseHeadIsALink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need a privilege here")
	}
	f := newFixture(t)
	wt, head := f.worktree("wt", "done")
	f.pr(1, "done", "MERGED", head, 2*time.Hour)
	bare := filepath.Join(wt, "target", "backup.git")
	git(t, f.root, "init", "-q", "--bare", bare)
	f.write(bare, filepath.Join("refs", "heads", "main"), head+"\n")
	if err := os.Remove(filepath.Join(bare, "HEAD")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("refs", "heads", "main"), filepath.Join(bare, "HEAD")); err != nil {
		t.Fatal(err)
	}
	wantVerdict(t, find(t, f.scan(), wt), Keep, "a repository inside build output: target/backup.git")

	// A HEAD linking to a branch with no commits yet dangles, and is still one.
	unborn := filepath.Join(wt, "target", "unborn.git")
	git(t, f.root, "init", "-q", "--bare", unborn)
	f.write(unborn, filepath.Join("refs", "heads", "saved"), head+"\n")
	if err := os.Remove(filepath.Join(unborn, "HEAD")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("refs", "heads", "new"), filepath.Join(unborn, "HEAD")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(bare); err != nil {
		t.Fatal(err)
	}
	wantVerdict(t, find(t, f.scan(), wt), Keep, "a repository inside build output: target/unborn.git")
}

func TestGCKeepsAWorktreeWithAJunctionAboveItsBuildOutput(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("git follows junctions on Windows; elsewhere a link is an untracked file")
	}
	f := newFixture(t)
	outside := filepath.Join(f.root, "elsewhere")
	f.write(outside, filepath.Join("target", "precious.txt"), "not this worktree's\n")
	wt, head := f.worktree("wt", "done")
	link(t, filepath.Join(wt, "x"), outside) // git status sees only !! x/target/
	f.pr(1, "done", "MERGED", head, 2*time.Hour)

	wantVerdict(t, find(t, f.scan(), wt), Keep, "a link out of the worktree above build output: x")
}

func TestGCKeepsTrackedFolderJunctionLeadingOut(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("git follows junctions on Windows; elsewhere a link changes a tracked folder")
	}
	f := newFixture(t)
	wt, _ := f.worktree("wt", "done")
	f.write(wt, filepath.Join("data", "item.txt"), "precious\n")
	git(t, wt, "add", "data")
	git(t, wt, "commit", "-q", "-m", "track data")
	head := git(t, wt, "rev-parse", "HEAD")
	outside := filepath.Join(f.root, "elsewhere")
	f.write(outside, "item.txt", "precious\n")
	if err := os.RemoveAll(filepath.Join(wt, "data")); err != nil {
		t.Fatal(err)
	}
	link(t, filepath.Join(wt, "data"), outside)
	f.pr(1, "done", "MERGED", head, 2*time.Hour)

	wts := f.scan()
	wantVerdict(t, find(t, wts, wt), Keep, "a link out of the worktree: data")
	removed, errs := Apply(context.Background(), wts)
	if len(removed) != 0 || len(errs) != 0 || !exists(filepath.Join(outside, "item.txt")) {
		t.Fatalf("removed %d, errors %v, outside copy exists: %v", len(removed), errs, exists(filepath.Join(outside, "item.txt")))
	}
}

func TestGCKeepsTrackedFolderSymlinkLeadingOutOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("git follows junctions on Windows; there the link walk keeps it")
	}
	f := newFixture(t)
	wt, _ := f.worktree("wt", "done")
	f.write(wt, filepath.Join("data", "item.txt"), "precious\n")
	git(t, wt, "add", "data")
	git(t, wt, "commit", "-q", "-m", "track data")
	head := git(t, wt, "rev-parse", "HEAD")
	outside := filepath.Join(f.root, "elsewhere")
	f.write(outside, "item.txt", "precious\n")
	if err := os.RemoveAll(filepath.Join(wt, "data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(wt, "data")); err != nil {
		t.Fatal(err)
	}
	f.pr(1, "done", "MERGED", head, 2*time.Hour)

	wts := f.scan()
	wantVerdict(t, find(t, wts, wt), Keep, "uncommitted")
	removed, errs := Apply(context.Background(), wts)
	if len(removed) != 0 || len(errs) != 0 || !exists(filepath.Join(outside, "item.txt")) {
		t.Fatalf("removed %d, errors %v, outside copy exists: %v", len(removed), errs, exists(filepath.Join(outside, "item.txt")))
	}
}

func TestGCLinkWalkKeepsOnlyDirectorySymlinksLeadingOutOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("git follows directory links only on Windows")
	}
	root := t.TempDir()
	wt := filepath.Join(root, "wt")
	outside := filepath.Join(root, "elsewhere")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "item.txt"), []byte("precious\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "item.txt"), filepath.Join(wt, "file-link")); err != nil {
		t.Skipf("symbolic links need a privilege here: %v", err)
	}
	if why := (&worktreeWalk{wt: wt}).linkRisk(); why != "" {
		t.Errorf("a file symlink leading out: got %q, want none (git deletes the link)", why)
	}

	if err := os.Symlink(outside, filepath.Join(wt, "dir-link")); err != nil {
		t.Fatal(err)
	}
	if why := (&worktreeWalk{wt: wt}).linkRisk(); why != "a link out of the worktree: dir-link" {
		t.Errorf("a directory symlink leading out: got %q", why)
	}
	if err := os.Remove(filepath.Join(wt, "dir-link")); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(filepath.Join(root, "missing"), filepath.Join(wt, "dangling")); err != nil {
		t.Fatal(err)
	}
	if why := (&worktreeWalk{wt: wt}).linkRisk(); why != "a link out of the worktree: dangling" {
		t.Errorf("a dangling link: got %q", why)
	}
}

func TestGCWalksBuildOutputReachedThroughAJunctionStayingInside(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("git follows junctions on Windows; elsewhere a link is an untracked file")
	}
	f := newFixture(t)
	outside := filepath.Join(f.root, "store")
	f.write(outside, "package.json", "{}\n")
	wt, head := f.worktree("wt", "done")
	link(t, filepath.Join(wt, "y", "target", "dep"), outside)
	link(t, filepath.Join(wt, "x"), filepath.Join(wt, "y")) // git status lists x/target/ first
	f.pr(1, "done", "MERGED", head, 2*time.Hour)

	wantVerdict(t, find(t, f.scan(), wt), Keep, "a link out of the worktree inside build output: x/target/dep")
}

func TestGCAllowsTrackedFolderJunctionStayingInside(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("git follows junctions on Windows; elsewhere a link changes a tracked folder")
	}
	f := newFixture(t)
	wt, _ := f.worktree("wt", "done")
	f.write(wt, filepath.Join("data", "item.txt"), "same\n")
	f.write(wt, filepath.Join("other", "item.txt"), "same\n")
	git(t, wt, "add", "data", "other")
	git(t, wt, "commit", "-q", "-m", "track folders")
	head := git(t, wt, "rev-parse", "HEAD")
	if err := os.RemoveAll(filepath.Join(wt, "data")); err != nil {
		t.Fatal(err)
	}
	link(t, filepath.Join(wt, "data"), filepath.Join(wt, "other"))
	f.pr(1, "done", "MERGED", head, 2*time.Hour)

	wantVerdict(t, find(t, f.scan(), wt), Remove, "merged")
}

func TestGCApplyKeepsTrackedFolderJunctionMadeAfterScan(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("git follows junctions on Windows; elsewhere a link changes a tracked folder")
	}
	f := newFixture(t)
	wt, _ := f.worktree("wt", "done")
	f.write(wt, filepath.Join("data", "item.txt"), "precious\n")
	git(t, wt, "add", "data")
	git(t, wt, "commit", "-q", "-m", "track data")
	head := git(t, wt, "rev-parse", "HEAD")
	f.pr(1, "done", "MERGED", head, 2*time.Hour)
	wts := f.scan()
	wantVerdict(t, find(t, wts, wt), Remove, "merged")

	outside := filepath.Join(f.root, "elsewhere")
	f.write(outside, "item.txt", "precious\n")
	if err := os.RemoveAll(filepath.Join(wt, "data")); err != nil {
		t.Fatal(err)
	}
	link(t, filepath.Join(wt, "data"), outside)
	removed, errs := Apply(context.Background(), wts)
	if len(removed) != 0 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "a link out of the worktree: data") {
		t.Fatalf("removed %d, errors %v; want the new junction kept", len(removed), errs)
	}
	if !exists(filepath.Join(outside, "item.txt")) {
		t.Error("the outside copy was deleted")
	}
}

func TestGCLinkWalkSkipsListedInnerWorktree(t *testing.T) {
	f := newFixture(t)
	outer, _ := f.worktree("wt-outer", "outer")
	inner, _ := f.worktree(filepath.Join(outer, ".hrn", "worktrees", "task"), "inner")
	outside := filepath.Join(f.root, "elsewhere")
	f.write(outside, "item.txt", "precious\n")
	link(t, filepath.Join(inner, "target", "dep"), outside)

	if why := (&worktreeWalk{wt: outer, registered: []string{outer, inner}}).linkRisk(); why != "" {
		t.Errorf("outer worktree found a risk inside its listed inner worktree: %s", why)
	}
}

func TestGCLinkWalkKeepsUnreadableWorktree(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if why := (&worktreeWalk{wt: missing}).linkRisk(); !strings.Contains(why, "the worktree cannot be read:") {
		t.Errorf("missing worktree: got %q, want an unreadable reason", why)
	}
}

func TestGCKeepsAWorktreeWithDotGitLinkLeadingOut(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("git follows junctions on Windows; elsewhere it unlinks a link")
	}
	f := newFixture(t)
	wt, _ := f.worktree("wt", "done")
	f.write(wt, filepath.Join("sub", "item.txt"), "tracked\n")
	git(t, wt, "add", "sub")
	git(t, wt, "commit", "-q", "-m", "track subfolder")
	head := git(t, wt, "rev-parse", "HEAD")
	outside := filepath.Join(f.root, "elsewhere")
	f.write(outside, "precious.txt", "outside\n")
	link(t, filepath.Join(wt, "sub", ".git"), outside)
	if status := git(t, wt, "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Skipf("git treats the nested .git link as a worktree change here: %s", status)
	}
	f.pr(1, "done", "MERGED", head, 2*time.Hour)

	wantVerdict(t, find(t, f.scan(), wt), Keep, "a link out of the worktree: sub/.git")
}

func TestGCRemovesAWorktreeWithATrackedSymlinkLeadingOutOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("git may not create tracked symlinks on Windows")
	}
	f := newFixture(t)
	wt, _ := f.worktree("wt", "done")
	outside := filepath.Join(f.root, "elsewhere")
	f.write(outside, "precious.txt", "outside\n")
	if err := os.Symlink(outside, filepath.Join(wt, "outside-link")); err != nil {
		t.Fatal(err)
	}
	git(t, wt, "add", "outside-link")
	git(t, wt, "commit", "-q", "-m", "track outside link")
	head := git(t, wt, "rev-parse", "HEAD")
	f.land("done")
	if status := git(t, wt, "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Fatalf("tracked symlink left the worktree dirty: %s", status)
	}
	f.pr(1, "done", "MERGED", head, 2*time.Hour)

	wts := f.scan()
	wantVerdict(t, find(t, wts, wt), Remove, "merged") // git unlinks a link here, without following it
	removed, errs := Apply(context.Background(), wts)
	if len(removed) != 1 || len(errs) != 0 || !exists(filepath.Join(outside, "precious.txt")) {
		t.Fatalf("removed %d, errors %v, outside copy exists: %v", len(removed), errs, exists(filepath.Join(outside, "precious.txt")))
	}
}

func TestGCKeepsBatchWorktreeWithTrackedFolderJunctionLeadingOut(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("git follows junctions on Windows; elsewhere a link changes a tracked folder")
	}
	f := newFixture(t)
	wt, _ := f.worktree(filepath.Join(f.repo, ".hrn", "worktrees", "task"), "hrn/task")
	f.write(wt, filepath.Join("data", "item.txt"), "precious\n")
	git(t, wt, "add", "data")
	git(t, wt, "commit", "-q", "-m", "track data")
	f.land("hrn/task")
	saveBatch(t, "junction", f.repo, batch.TaskResult{ID: "task", Status: batch.StatusPass, Branch: "hrn/task", Worktree: wt})
	outside := filepath.Join(f.root, "elsewhere")
	f.write(outside, "item.txt", "precious\n")
	if err := os.RemoveAll(filepath.Join(wt, "data")); err != nil {
		t.Fatal(err)
	}
	link(t, filepath.Join(wt, "data"), outside)

	wantVerdict(t, find(t, f.scan(), wt), Keep, "a link out of the worktree: data")
}

func TestGCKeepsSquashLandedBatchWorktreeWithTrackedFolderJunctionLeadingOut(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("git follows junctions on Windows; elsewhere a link changes a tracked folder")
	}
	f := newFixture(t)
	wt, _ := f.worktree(filepath.Join(f.repo, ".hrn", "worktrees", "task"), "hrn/task")
	f.write(wt, filepath.Join("data", "item.txt"), "precious\n")
	git(t, wt, "add", "data")
	git(t, wt, "commit", "-q", "-m", "track data")
	saveBatch(t, "squash-junction", f.repo, batch.TaskResult{ID: "task", Status: batch.StatusPass, Branch: "hrn/task", Worktree: wt})
	f.write(f.repo, "hrn/task.txt", "hrn/task\n")
	f.write(f.repo, filepath.Join("data", "item.txt"), "precious\n")
	git(t, f.repo, "add", ".")
	git(t, f.repo, "commit", "-q", "-m", "reviewed wording")
	git(t, f.repo, "push", "-q", "origin", "main")

	outside := filepath.Join(f.root, "elsewhere")
	f.write(outside, "item.txt", "precious\n")
	if err := os.RemoveAll(filepath.Join(wt, "data")); err != nil {
		t.Fatal(err)
	}
	link(t, filepath.Join(wt, "data"), outside)

	wantVerdict(t, find(t, f.scan(), wt), Keep, "a link out of the worktree: data")
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
	wantVerdict(t, find(t, wts, landed), Remove, "batch 20261011-100000-aaaaaa finished and its commit is on origin/main")
	wantVerdict(t, find(t, wts, unlanded), Keep, "not on origin/main")
	wantVerdict(t, find(t, wts, running), Keep, "still running")
}

func TestGCRemovesBatchChangeLandedAsRewordedSquash(t *testing.T) {
	f := newFixture(t)
	task, _ := f.worktree(filepath.Join(f.repo, ".hrn", "worktrees", "task"), "hrn/task")
	saveBatch(t, "squash", f.repo, batch.TaskResult{ID: "task", Status: batch.StatusPass, Branch: "hrn/task", Worktree: task})
	f.write(f.repo, "hrn/task.txt", "hrn/task\n")
	git(t, f.repo, "add", ".")
	git(t, f.repo, "commit", "-q", "-m", "reviewed wording")
	landed := git(t, f.repo, "rev-parse", "HEAD")
	git(t, f.repo, "push", "-q", "origin", "main")

	wantVerdict(t, find(t, f.scan(), task), Remove, "landed on origin/main as "+landed[:7])
}

func TestGCRemovesTwoCommitBatchChangeLandedAsOneCommit(t *testing.T) {
	f := newFixture(t)
	task, _ := f.worktree(filepath.Join(f.repo, ".hrn", "worktrees", "task"), "hrn/task")
	f.write(task, "second.txt", "second\n")
	git(t, task, "add", ".")
	git(t, task, "commit", "-q", "-m", "second task commit")
	saveBatch(t, "two-commits", f.repo, batch.TaskResult{ID: "task", Status: batch.StatusPass, Branch: "hrn/task", Worktree: task})
	f.write(f.repo, "hrn/task.txt", "hrn/task\n")
	f.write(f.repo, "second.txt", "second\n")
	git(t, f.repo, "add", ".")
	git(t, f.repo, "commit", "-q", "-m", "one landed commit")
	git(t, f.repo, "push", "-q", "origin", "main")

	wantVerdict(t, find(t, f.scan(), task), Remove, "landed on origin/main as")
}

func TestGCRemovesBatchChangeAfterLaterEditToSameFile(t *testing.T) {
	f := newFixture(t)
	task, _ := f.worktree(filepath.Join(f.repo, ".hrn", "worktrees", "task"), "hrn/task")
	saveBatch(t, "later-edit", f.repo, batch.TaskResult{ID: "task", Status: batch.StatusPass, Branch: "hrn/task", Worktree: task})
	f.write(f.repo, "hrn/task.txt", "hrn/task\n")
	git(t, f.repo, "add", ".")
	git(t, f.repo, "commit", "-q", "-m", "land task")
	landed := git(t, f.repo, "rev-parse", "HEAD")
	f.write(f.repo, "hrn/task.txt", "hrn/task\nlater edit\n")
	git(t, f.repo, "add", ".")
	git(t, f.repo, "commit", "-q", "-m", "later edit")
	git(t, f.repo, "push", "-q", "origin", "main")

	wantVerdict(t, find(t, f.scan(), task), Remove, "landed on origin/main as "+landed[:7])
}

func TestGCKeepsBatchChangeAtCommitComparisonBound(t *testing.T) {
	previous := maxCommits
	maxCommits = 2
	t.Cleanup(func() { maxCommits = previous })
	f := newFixture(t)
	task, _ := f.worktree(filepath.Join(f.repo, ".hrn", "worktrees", "task"), "hrn/task")
	saveBatch(t, "bound", f.repo, batch.TaskResult{ID: "task", Status: batch.StatusPass, Branch: "hrn/task", Worktree: task})
	f.write(f.repo, "hrn/task.txt", "hrn/task\n")
	git(t, f.repo, "add", ".")
	git(t, f.repo, "commit", "-q", "-m", "land task")
	f.write(f.repo, "later.txt", "later\n")
	git(t, f.repo, "add", ".")
	git(t, f.repo, "commit", "-q", "-m", "later change")
	git(t, f.repo, "push", "-q", "origin", "main")

	wantVerdict(t, find(t, f.scan(), task), Keep, "not on origin/main")
}

func TestGCKeepsBatchChangeThatLandedDifferently(t *testing.T) {
	for _, landed := range []string{"hrn/task\nreview edit\n", "review edit\n"} {
		t.Run(strings.TrimSpace(landed), func(t *testing.T) {
			f := newFixture(t)
			task, _ := f.worktree(filepath.Join(f.repo, ".hrn", "worktrees", "task"), "hrn/task")
			f.write(task, "hrn/task.txt", "hrn/task\nsecond line\n")
			git(t, task, "add", ".")
			git(t, task, "commit", "-q", "-m", "second task commit")
			saveBatch(t, "different", f.repo, batch.TaskResult{ID: "task", Status: batch.StatusPass, Branch: "hrn/task", Worktree: task})
			f.write(f.repo, "hrn/task.txt", landed)
			git(t, f.repo, "add", ".")
			git(t, f.repo, "commit", "-q", "-m", "different landing")
			git(t, f.repo, "push", "-q", "origin", "main")

			wantVerdict(t, find(t, f.scan(), task), Keep, "not on origin/main")
		})
	}
}

func TestGCKeepsBatchReindentThatLandedWithDifferentWhitespace(t *testing.T) {
	f := newFixture(t)
	f.write(f.repo, "README.md", "  hello\n")
	git(t, f.repo, "commit", "-q", "-am", "original indent")
	git(t, f.repo, "push", "-q", "origin", "main")
	task := filepath.Join(f.repo, ".hrn", "worktrees", "task")
	git(t, f.repo, "worktree", "add", "-q", "-b", "hrn/task", task, "main")
	f.write(task, "README.md", "    hello\n")
	git(t, task, "commit", "-q", "-am", "task reindent")
	saveBatch(t, "reindent", f.repo, batch.TaskResult{ID: "task", Status: batch.StatusPass, Branch: "hrn/task", Worktree: task})
	f.write(f.repo, "README.md", "\thello\n")
	git(t, f.repo, "commit", "-q", "-am", "different reindent")
	git(t, f.repo, "push", "-q", "origin", "main")

	wantVerdict(t, find(t, f.scan(), task), Keep, "not on origin/main")
}

func TestGCKeepsBatchChangeWhenScriptModeDidNotLand(t *testing.T) {
	f := newFixture(t)
	f.write(f.repo, "script.sh", "echo before\n")
	git(t, f.repo, "add", ".")
	git(t, f.repo, "commit", "-q", "-m", "original script")
	git(t, f.repo, "push", "-q", "origin", "main")
	task := filepath.Join(f.repo, ".hrn", "worktrees", "task")
	git(t, f.repo, "worktree", "add", "-q", "-b", "hrn/task", task, "main")
	f.write(task, "script.sh", "echo after\n")
	git(t, task, "add", "script.sh")
	git(t, task, "update-index", "--chmod=+x", "script.sh")
	// The file on disk follows the index, or git on Linux sees the mode as an
	// uncommitted change and the worktree is kept for that instead.
	if err := os.Chmod(filepath.Join(task, "script.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, task, "commit", "-q", "-m", "change script and mode")
	saveBatch(t, "mode", f.repo, batch.TaskResult{ID: "task", Status: batch.StatusPass, Branch: "hrn/task", Worktree: task})
	f.write(f.repo, "script.sh", "echo after\n")
	git(t, f.repo, "commit", "-q", "-am", "change script content")
	git(t, f.repo, "push", "-q", "origin", "main")

	wantVerdict(t, find(t, f.scan(), task), Keep, "not on origin/main")
}

func TestGCComparesBatchChangesWithoutTextconv(t *testing.T) {
	f := newFixture(t)
	f.write(f.repo, ".gitattributes", "asset.txt diff=mask\n")
	f.write(f.repo, "asset.txt", "before\n")
	git(t, f.repo, "add", ".")
	git(t, f.repo, "commit", "-q", "-m", "original asset")
	git(t, f.repo, "push", "-q", "origin", "main")
	git(t, f.repo, "config", "diff.mask.textconv", "echo masked")
	task := filepath.Join(f.repo, ".hrn", "worktrees", "task")
	git(t, f.repo, "worktree", "add", "-q", "-b", "hrn/task", task, "main")
	f.write(task, "asset.txt", "after\n")
	git(t, task, "commit", "-q", "-am", "task asset")
	saveBatch(t, "textconv", f.repo, batch.TaskResult{ID: "task", Status: batch.StatusPass, Branch: "hrn/task", Worktree: task})
	f.write(f.repo, "asset.txt", "after\n")
	git(t, f.repo, "commit", "-q", "-am", "land asset")
	git(t, f.repo, "push", "-q", "origin", "main")

	wantVerdict(t, find(t, f.scan(), task), Remove, "landed on origin/main as")
}

func TestGCKeepsBatchTasksCombinedIntoOneCommit(t *testing.T) {
	f := newFixture(t)
	a, _ := f.worktree(filepath.Join(f.repo, ".hrn", "worktrees", "a"), "hrn/a")
	b, _ := f.worktree(filepath.Join(f.repo, ".hrn", "worktrees", "b"), "hrn/b")
	saveBatch(t, "combined", f.repo,
		batch.TaskResult{ID: "a", Status: batch.StatusPass, Branch: "hrn/a", Worktree: a},
		batch.TaskResult{ID: "b", Status: batch.StatusPass, Branch: "hrn/b", Worktree: b})
	f.write(f.repo, "hrn/a.txt", "hrn/a\n")
	f.write(f.repo, "hrn/b.txt", "hrn/b\n")
	git(t, f.repo, "add", ".")
	git(t, f.repo, "commit", "-q", "-m", "combine tasks")
	git(t, f.repo, "push", "-q", "origin", "main")

	wts := f.scan()
	wantVerdict(t, find(t, wts, a), Keep, "not on origin/main")
	wantVerdict(t, find(t, wts, b), Keep, "not on origin/main")
}

func TestGCKeepsBatchTaskWithEmptyDiff(t *testing.T) {
	f := newFixture(t)
	task := filepath.Join(f.repo, ".hrn", "worktrees", "task")
	git(t, f.repo, "worktree", "add", "-q", "-b", "hrn/task", task, "main")
	git(t, task, "commit", "-q", "--allow-empty", "-m", "empty task")
	saveBatch(t, "empty", f.repo, batch.TaskResult{ID: "task", Status: batch.StatusPass, Branch: "hrn/task", Worktree: task})
	f.write(f.repo, "later.txt", "later\n")
	git(t, f.repo, "add", ".")
	git(t, f.repo, "commit", "-q", "-m", "later change")
	git(t, f.repo, "push", "-q", "origin", "main")
	base := git(t, f.repo, "merge-base", "hrn/task", "main")
	if got := git(t, f.repo, "rev-list", "--count", "--no-merges", base+"..main"); got != "1" {
		t.Fatalf("main has %s new commits, want 1 so the empty-diff path is reached", got)
	}

	wantVerdict(t, find(t, f.scan(), task), Keep, "not on origin/main")
}

func TestGCReadsLandedPatchIDsOncePerMergeBaseAsBefore(t *testing.T) {
	f := newFixture(t)
	a, _ := f.worktree(filepath.Join(f.repo, ".hrn", "worktrees", "a"), "hrn/a")
	b, _ := f.worktree(filepath.Join(f.repo, ".hrn", "worktrees", "b"), "hrn/b")
	f.write(f.repo, "hrn/a.txt", "hrn/a\n")
	git(t, f.repo, "add", ".")
	git(t, f.repo, "commit", "-q", "-m", "land a")
	landedA := git(t, f.repo, "rev-parse", "HEAD")
	c, _ := f.worktree(filepath.Join(f.repo, ".hrn", "worktrees", "c"), "hrn/c")
	f.write(c, "asset.bin", "\x00\x01\x02\xff")
	git(t, c, "add", ".")
	git(t, c, "commit", "-q", "-m", "c binary")
	f.write(f.repo, "asset.bin", "\x00\x01\x02\xff")
	f.write(f.repo, "hrn/c.txt", "hrn/c\n")
	git(t, f.repo, "add", ".")
	git(t, f.repo, "commit", "-q", "-m", "land c with a binary")
	git(t, f.repo, "push", "-q", "origin", "main")
	saveBatch(t, "shared", f.repo,
		batch.TaskResult{ID: "a", Status: batch.StatusPass, Branch: "hrn/a", Worktree: a},
		batch.TaskResult{ID: "b", Status: batch.StatusPass, Branch: "hrn/b", Worktree: b},
		batch.TaskResult{ID: "c", Status: batch.StatusPass, Branch: "hrn/c", Worktree: c})

	previous := landedPatchIDs
	t.Cleanup(func() { landedPatchIDs = previous })
	calls := map[string]int{}
	landedPatchIDs = func(ctx context.Context, repo, base, def string) []patchID {
		calls[base]++
		return previous(ctx, repo, base, def)
	}
	wts := f.scan()
	wantVerdict(t, find(t, wts, a), Remove, "landed on origin/main as "+landedA[:7])
	wantVerdict(t, find(t, wts, b), Keep, "not on origin/main")
	wantVerdict(t, find(t, wts, c), Remove, "landed on origin/main as")

	t.Run("once per merge base", func(t *testing.T) {
		if len(calls) != 2 {
			t.Fatalf("patch ids read for %d merge bases, want 2: %v", len(calls), calls)
		}
		for base, n := range calls {
			if n != 1 {
				t.Errorf("patch ids for %s read %d times, want once", base, n)
			}
		}
	})
	t.Run("same ids as the whole log read at once", func(t *testing.T) {
		ctx := context.Background()
		def := "refs/remotes/origin/main"
		for base := range calls {
			patches, err := gitStdout(ctx, f.repo, "log", "-p", "--no-ext-diff", "--no-textconv", "--submodule=short", "--no-merges", "--binary", "--format=commit %H", "--max-count="+fmt.Sprint(maxCommits), base+".."+def)
			if err != nil {
				t.Fatal(err)
			}
			want, err := gitStdoutInput(ctx, f.repo, patches, "patch-id", "--verbatim")
			if err != nil {
				t.Fatal(err)
			}
			got, err := logPatchIDs(ctx, f.repo, base, def)
			if err != nil {
				t.Fatal(err)
			}
			if got != want || strings.TrimSpace(got) == "" {
				t.Errorf("base %s: streamed ids\n%s\nwant\n%s", base, got, want)
			}
		}
	})
}

// landedTask lands a batch task's change on origin/main as a squash, after a
// commit large enough to fill the pipe between git log and git patch-id.
func landedTask(t *testing.T, f *fixture) string {
	t.Helper()
	task := filepath.Join(f.repo, ".hrn", "worktrees", "task")
	git(t, f.repo, "worktree", "add", "-q", "-b", "hrn/task", task, "main")
	f.write(task, "task.txt", "task\n")
	git(t, task, "add", ".")
	git(t, task, "commit", "-q", "-m", "task")
	saveBatch(t, "landed", f.repo, batch.TaskResult{ID: "task", Status: batch.StatusPass, Branch: "hrn/task", Worktree: task})
	f.write(f.repo, "big.txt", strings.Repeat("a line of the big file\n", 1<<16))
	git(t, f.repo, "add", ".")
	git(t, f.repo, "commit", "-q", "-m", "big file")
	f.write(f.repo, "task.txt", "task\n")
	git(t, f.repo, "add", ".")
	git(t, f.repo, "commit", "-q", "-m", "land task")
	git(t, f.repo, "push", "-q", "origin", "main")
	return task
}

// logPatchIDsWithin calls logPatchIDs and fails the test if it outlasts a
// minute, well inside its own two-minute limit.
func logPatchIDsWithin(t *testing.T, repo, base string) error {
	t.Helper()
	start := time.Now()
	_, err := logPatchIDs(context.Background(), repo, base, "refs/remotes/origin/main")
	if d := time.Since(start); d > time.Minute {
		t.Fatalf("logPatchIDs took %s", d)
	}
	return err
}

func TestGCKeepsLandedBatchChangeWhenGitLogFails(t *testing.T) {
	f := newFixture(t)
	task := landedTask(t, f)

	err := logPatchIDsWithin(t, f.repo, "refs/heads/no-such-base")
	if err == nil || !strings.Contains(err.Error(), "git log") {
		t.Fatalf("logPatchIDs with a bad base: %v, want a git log error", err)
	}

	// rev-list and merge-base ignore log.* settings, so only git log fails.
	git(t, f.repo, "config", "log.showRoot", "bogus")
	wantVerdict(t, find(t, f.scan(), task), Keep, "not on origin/main")
	git(t, f.repo, "config", "--unset", "log.showRoot")
	wantVerdict(t, find(t, f.scan(), task), Remove, "landed on origin/main as")
}

func TestGCKeepsLandedBatchChangeWhenGitPatchIDFails(t *testing.T) {
	f := newFixture(t)
	task := landedTask(t, f)
	git(t, f.repo, "config", "patchid.stable", "bogus")
	base := git(t, f.repo, "merge-base", "hrn/task", "main")

	err := logPatchIDsWithin(t, f.repo, base)
	if err == nil || !strings.Contains(err.Error(), "git patch-id") {
		t.Fatalf("logPatchIDs with a failing patch-id: %v, want a git patch-id error", err)
	}

	wantVerdict(t, find(t, f.scan(), task), Keep, "not on origin/main")
	git(t, f.repo, "config", "--unset", "patchid.stable")
	wantVerdict(t, find(t, f.scan(), task), Remove, "landed on origin/main as")
}

func TestGCRemovesBinaryBatchChangeLandedAsSquash(t *testing.T) {
	f := newFixture(t)
	task := filepath.Join(f.repo, ".hrn", "worktrees", "task")
	git(t, f.repo, "worktree", "add", "-q", "-b", "hrn/task", task, "main")
	f.write(task, "asset.bin", "\x00\x01\x02\xff")
	git(t, task, "add", ".")
	git(t, task, "commit", "-q", "-m", "task binary")
	saveBatch(t, "binary", f.repo, batch.TaskResult{ID: "task", Status: batch.StatusPass, Branch: "hrn/task", Worktree: task})
	f.write(f.repo, "asset.bin", "\x00\x01\x02\xff")
	git(t, f.repo, "add", ".")
	git(t, f.repo, "commit", "-q", "-m", "land binary")
	git(t, f.repo, "push", "-q", "origin", "main")

	wantVerdict(t, find(t, f.scan(), task), Remove, "landed on origin/main as")
}

func TestGCKeepsBatchChangeWhenMainAddsDifferentBinaryAtSamePath(t *testing.T) {
	f := newFixture(t)
	task := filepath.Join(f.repo, ".hrn", "worktrees", "task")
	git(t, f.repo, "worktree", "add", "-q", "-b", "hrn/task", task, "main")
	f.write(task, "asset.bin", "\x00\x01\x02\xff")
	git(t, task, "add", ".")
	git(t, task, "commit", "-q", "-m", "task binary")
	saveBatch(t, "different-binary", f.repo, batch.TaskResult{ID: "task", Status: batch.StatusPass, Branch: "hrn/task", Worktree: task})
	f.write(f.repo, "asset.bin", "\x00\x01\x03\xff")
	git(t, f.repo, "add", ".")
	git(t, f.repo, "commit", "-q", "-m", "different binary")
	git(t, f.repo, "push", "-q", "origin", "main")

	wantVerdict(t, find(t, f.scan(), task), Keep, "not on origin/main")
}

// A batch task's slot is a locked worktree that outlives the batch: even when
// the task's commit has landed, the slot stays for the next take.
func TestGCKeepsABatchSlotWhoseCommitLandedBecauseItIsLocked(t *testing.T) {
	f := newFixture(t)
	slot, _ := f.worktree(filepath.Join(f.repo, ".hrn", "slots", "1"), "hrn/landed")
	git(t, f.repo, "worktree", "lock", "--reason", "hrn ws slot", slot)
	f.land("hrn/landed")
	saveBatch(t, "20261013-100000-aaaaaa", f.repo,
		batch.TaskResult{ID: "landed", Status: batch.StatusPass, Branch: "hrn/landed", Worktree: slot, Slot: 1})

	wantVerdict(t, find(t, f.scan(), slot), Keep, "locked")
	if !exists(slot) {
		t.Error("the slot must stay")
	}
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

func TestGCFixtureGitIgnoresGuardConfigPassedDownInItsEnvironment(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url.push-refused-by-hrn://.pushinsteadof")
	t.Setenv("GIT_CONFIG_VALUE_0", "")

	dir, err := buildTemplate()
	if dir != "" {
		defer os.RemoveAll(dir)
	}
	if err != nil {
		t.Fatalf("the template build saw the guard config: %v", err)
	}
	f := newFixture(t)
	wt, head := f.worktree("wt", "branch")
	if !exists(wt) || head == "" {
		t.Fatal("the fixture did not create its worktree")
	}
	f.land("branch")
}

func TestGCIgnoresGitConfigPassedDownInItsEnvironment(t *testing.T) {
	for _, form := range []string{"numbered", "parameters"} {
		t.Run(form, func(t *testing.T) {
			f := newFixture(t)
			mirror := filepath.Join(f.root, "mirror.git")
			git(t, f.root, "clone", "-q", "--bare", f.origin, mirror)
			task, _ := f.worktree(filepath.Join(f.repo, ".hrn", "worktrees", "task"), "hrn/task")
			git(t, f.repo, "push", "-q", mirror, "hrn/task:main") // only the mirror has the task's commit
			saveBatch(t, "20261011-100000-aaaaaa", f.repo,
				batch.TaskResult{ID: "task", Status: batch.StatusPass, Branch: "hrn/task", Worktree: task})
			key, value := "url."+filepath.ToSlash(mirror)+".insteadOf", f.origin
			if form == "numbered" {
				t.Setenv("GIT_CONFIG_COUNT", "1")
				t.Setenv("GIT_CONFIG_KEY_0", key)
				t.Setenv("GIT_CONFIG_VALUE_0", value)
			} else {
				t.Setenv("GIT_CONFIG_PARAMETERS", "'"+key+"'='"+value+"'")
			}

			wantVerdict(t, find(t, f.scan(), task), Keep, "not on origin/main")
		})
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

func TestGCApplyLeavesAWorktreeThatSwitchedBranchAtTheSameCommit(t *testing.T) {
	f := newFixture(t)
	wt, head := f.worktree("wt", "done")
	f.pr(1, "done", "MERGED", head, 2*time.Hour)
	wts := f.scan()
	wantVerdict(t, find(t, wts, wt), Remove, "merged")

	git(t, wt, "switch", "-q", "-c", "next-piece")
	removed, errs := Apply(context.Background(), wts)
	if len(removed) != 0 || len(errs) != 1 || !exists(wt) {
		t.Fatalf("removed %d, errors %v; want the worktree on its new branch kept", len(removed), errs)
	}
}

func TestGCKeepsAWorktreeWhoseHrnWorktreesIsAFile(t *testing.T) {
	f := newFixture(t)
	wt, head := f.worktree("wt", "done")
	f.write(wt, filepath.Join(".hrn", "worktrees"), "not what hrn writes\n")
	f.pr(1, "done", "MERGED", head, 2*time.Hour)

	wantVerdict(t, find(t, f.scan(), wt), Keep, ".hrn/worktrees")
}

func TestGCReadsAWorktreePathWithANewline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows paths cannot hold a newline")
	}
	f := newFixture(t)
	odd, head := f.worktree("wt\nHEAD 0000000000000000000000000000000000000000", "done")
	f.pr(1, "done", "MERGED", head, 2*time.Hour)

	wts := f.scan()
	if len(wts) != 1 {
		t.Fatalf("want one worktree, got %+v", wts)
	}
	wantVerdict(t, find(t, wts, odd), Remove, "merged")
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
