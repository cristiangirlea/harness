package watch

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestRunsAreGroupedByTheirRemoteEvenAfterTheirFolderIsGone(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	root := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	mkdir := func(parts ...string) string {
		t.Helper()
		p := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// A run records its remote, then its worktree is deleted.
	gone := mkdir("wt-review")
	git(gone, "init")
	git(gone, "config", "user.email", "test@example.test")
	git(gone, "config", "user.name", "Test")
	git(gone, "remote", "add", "origin", "https://user:s3cret@github.com/acme/harness.git")
	git(gone, "commit", "--allow-empty", "-m", "initial")
	saveRunIn(t, gone, "review the diff", agent.Result{CostUSD: 0.25})
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	// An older record, in a checkout whose config names the same remote.
	old := mkdir("harness", ".git")
	if err := os.WriteFile(filepath.Join(old, "config"), []byte("[remote \"origin\"]\n\turl = git@github.com:acme/harness.git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	saveRunIn(t, filepath.Dir(old), "fix the login bug", agent.Result{CostUSD: 0.5})
	// A bare repository with its own remote, reached through a worktree.
	bare := mkdir("tools.git")
	mkdir("tools.git", "worktrees", "wt-tools")
	for name, text := range map[string]string{"HEAD": "ref: refs/heads/main\n", "config": "[remote \"upstream\"]\n\turl = https://gitlab.example.com/group/tools.git\n"} {
		if err := os.WriteFile(filepath.Join(bare, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wtTools := mkdir("wt-tools")
	if err := os.WriteFile(filepath.Join(wtTools, ".git"), []byte("gitdir: "+filepath.ToSlash(filepath.Join(bare, "worktrees", "wt-tools"))), 0o644); err != nil {
		t.Fatal(err)
	}
	saveRunIn(t, wtTools, "add a cache", agent.Result{})
	// Neither a repository with a remote nor a record of one.
	saveRunIn(t, mkdir("notes"), "plan the week", agent.Result{})
	mkdir("local-only", ".git")
	saveRunIn(t, filepath.Join(root, "local-only"), "plan the sprint", agent.Result{})
	saveRunIn(t, filepath.Join(root, "deleted-scratch"), "plan the release", agent.Result{})

	c := cardOf(t)
	var rows []string
	for _, r := range c.Repos {
		rows = append(rows, r.Row.Purpose+"="+r.Row.Runs)
	}
	if want := "acme/harness=2|gitlab.example.com/group/tools=1|No repository recorded=3"; strings.Join(rows, "|") != want {
		t.Errorf("repository rows = %q, want %q", strings.Join(rows, "|"), want)
	}
}

func TestARecordedRepositoryIsCheckedAgainWhenRead(t *testing.T) {
	root := t.TempDir()
	for repo, want := range map[string]string{
		"github.com/acme/tool":                     "github.com/acme/tool",
		`github.com/acme/tool"><script>x</script>`: "",
		"github.com/acme/../tool":                  "",
		"javascript:alert(1)//x/y":                 "",
		"github.com/acme/tool.git":                 "github.com/acme/tool.git",
	} {
		if _, name := repoOfRun(runlog.Record{Repo: repo, Task: agent.Task{Dir: filepath.Join(root, "gone")}}); name != want {
			t.Errorf("recorded %q reads as %q, want %q", repo, name, want)
		}
	}
}

func TestTheRunPageLinksTheBranchCommitsAndPullRequests(t *testing.T) {
	commits := make([]string, commitLinks+2)
	for i := range commits {
		commits[i] = fmt.Sprintf("%040x", i+1)
	}
	start := strings.Repeat("ab", 20)
	rec := runlog.Record{ID: "20261106-120000-abcdef", Started: time.Now(), Done: true, Repo: "github.com/acme/tool", Branch: "feat/fix-1", StartHead: start,
		Result: agent.Result{Backend: "codex", Status: agent.StatusOK, Commits: commits, EndHead: commits[0]}}
	html, err := render("run", runPageOf(rec, nil, time.Time{}, viewer{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<a href="https://github.com/acme/tool" rel="noreferrer noopener">acme/tool</a>`,
		`<a href="https://github.com/acme/tool/tree/feat/fix-1" rel="noreferrer noopener">feat/fix-1</a>`,
		`href="https://github.com/acme/tool/pulls?q=is%3Apr`, `head%3Afeat%2Ffix-1"`, `>pull requests</a>`,
		`<a href="https://github.com/acme/tool/commit/` + start + `" rel="noreferrer noopener">abababa</a>`,
		`<a href="https://github.com/acme/tool/commit/` + commits[0] + `" rel="noreferrer noopener">0000000</a>`,
		`<a href="https://github.com/acme/tool/commit/` + commits[commitLinks-1] + `"`,
		`<a href="https://github.com/acme/tool/compare/` + start + `...` + commits[0] + `" rel="noreferrer noopener">and 2 more</a>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the run page lacks %q", want)
		}
	}
	if strings.Contains(html, commits[commitLinks]) {
		t.Errorf("the page links more than %d commits", commitLinks)
	}

	// A hand-edited branch is escaped in the text and in the links.
	rec.Branch = `x"><script>alert(1)</script>`
	if html, err = render("run", runPageOf(rec, nil, time.Time{}, viewer{})); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(html, "<script>alert(1)") || strings.Contains(html, `x"><`) {
		t.Error("a branch name reached the page unescaped")
	}

	// Another host gets the names without links.
	rec.Repo = "gitlab.example.com/group/tool"
	html, err = render("run", runPageOf(rec, nil, time.Time{}, viewer{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, "gitlab.example.com/group/tool") || strings.Contains(html, `href="https://gitlab`) || strings.Contains(html, "pull requests") {
		t.Error("a repository off github.com should show as plain text")
	}
	if strings.Contains(html, "<script>alert(1)") {
		t.Error("a branch name reached the page unescaped")
	}
}

func TestARunOutsideAnyRepositoryHasNoGitFacts(t *testing.T) {
	rec := runlog.Record{ID: "20261106-120000-abcdef", Started: time.Now(), Task: agent.Task{Dir: t.TempDir()}}
	if facts := gitFacts(rec); len(facts) != 0 {
		t.Errorf("facts = %+v, want none", facts)
	}
}

func TestAnOlderRecordGetsNoLinksFromWhatItsFolderHoldsToday(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "harness")
	withRemote(t, dir, "https://github.com/acme/other.git")
	rec := runlog.Record{ID: "20261106-120000-abcdef", Started: time.Now(), Done: true, StartHead: strings.Repeat("ab", 20), Task: agent.Task{Dir: dir},
		Result: agent.Result{Backend: "codex", Status: agent.StatusOK, Commits: []string{strings.Repeat("cd", 20)}}}
	if _, name := repoOfRun(rec); name != "github.com/acme/other" {
		t.Errorf("the card files it under %q, want the checkout's remote", name)
	}
	html, err := render("run", runPageOf(rec, nil, time.Time{}, viewer{}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(html, "acme/other") || strings.Contains(html, `href="https://github.com`) {
		t.Error("the run page links a repository the run never recorded")
	}
}

func TestRepositoryCaseFoldsOnlyOnGitHub(t *testing.T) {
	key := func(repo string) string { k, _ := recordedRepo(runlog.Record{Repo: repo}); return k }
	if key("github.com/Acme/Tool") != key("github.com/acme/tool") {
		t.Error("GitHub names differing only in case should share a row")
	}
	if key("git.example.com/acme/Tool") == key("git.example.com/acme/tool") {
		t.Error("another server's names differing in case should stay apart")
	}
}

func TestACheckoutsRemoteComesFromItsGitDirOnly(t *testing.T) {
	root := t.TempDir()
	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	origin := "[remote \"origin\"] # where it came from\n\turl = \"https://github.com/acme/app.git\" ; the fork\n"
	// A submodule's .git file names its git directory, whose config holds the remote.
	write(filepath.Join(root, "app", ".git", "config"), origin)
	write(filepath.Join(root, "app", ".git", "modules", "lib", "config"), "[remote\t\"origin\"]\n\turl = git@github.com:acme/lib.git\n")
	write(filepath.Join(root, "app", "vendor", "lib", ".git"), "gitdir: ../../.git/modules/lib\n")
	// A checkout whose own top folder has a file named config: not git's.
	write(filepath.Join(root, "site", "config"), origin)
	if err := os.MkdirAll(filepath.Join(root, "site", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]string{
		filepath.Join(root, "app"):                  "github.com/acme/app",
		filepath.Join(root, "app", "vendor", "lib"): "github.com/acme/lib",
		filepath.Join(root, "site"):                 "",
	} {
		if _, name := repoOfDir(dir); name != want {
			t.Errorf("repoOfDir(%q) = %q, want %q", dir, name, want)
		}
	}
}
