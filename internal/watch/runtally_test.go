package watch

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/owner"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// saveRunIn is saveRun for a run started in dir.
func saveRunIn(t *testing.T, dir, prompt string, res agent.Result) string {
	t.Helper()
	w, err := runlog.Create("claude", agent.Task{Prompt: prompt, Dir: dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Backend = "claude"
	if res.Status == "" {
		res.Status = agent.StatusOK
	}
	if err := w.Close(res); err != nil {
		t.Fatal(err)
	}
	return w.ID()
}

func repoRowOf(t *testing.T, c *purposeCard, name string) repoRow {
	t.Helper()
	if c == nil {
		t.Fatal("no card")
	}
	for _, r := range c.Repos {
		if r.Row.Purpose == name {
			return r
		}
	}
	t.Fatalf("the card has no %q repository row: %+v", name, c.Repos)
	return repoRow{}
}

func TestThePurposeCardCoversEveryRunNotOnlyTheListedOnes(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	saveRun(t, "claude", "review the diff", &agent.Result{CostUSD: 0.01})
	time.Sleep(1100 * time.Millisecond) // run ids are ordered by the second they start
	saveRun(t, "claude", "fix the login bug", &agent.Result{CostUSD: 0.01})
	saveRun(t, "codex", "add a cache", &agent.Result{})

	srv := httptest.NewServer(New(context.Background(), Options{Runs: 2, Poll: 5 * time.Millisecond}))
	defer srv.Close()
	_, page := get(t, srv.URL+"/")
	if !strings.Contains(page, "All 3 recorded runs, not only the ones listed below.") {
		t.Error("the card does not say it covers every recorded run")
	}
	all := strings.Join([]string{`<tr class="all">`, `<th scope="row">all</th>`, `<td class="num">3</td>`}, "\n")
	if !strings.Contains(page, all) {
		t.Error("the card's all row does not count 3 runs")
	}
	if !strings.Contains(page, `<th scope="row">review</th>`) {
		t.Error("the card has no row for the oldest run, which the index does not list")
	}
	if got := strings.Count(page, `class="meta purpose"`); got != 2 {
		t.Errorf("the index lists %d runs, want 2", got)
	}
}

func TestRepositoryNamesFollowWorktreesBackToTheirCheckout(t *testing.T) {
	root := t.TempDir()
	mkdir := func(parts ...string) string {
		p := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write := func(path, text string) {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	main := mkdir("harness")
	mkdir("harness", ".git", "worktrees", "wt-x")
	wt := mkdir("scratch", "wt-x")
	write(filepath.Join(wt, ".git"), "gitdir: "+filepath.ToSlash(filepath.Join(main, ".git", "worktrees", "wt-x"))+"\n")
	backslashed := mkdir("scratch", "wt-y")
	write(filepath.Join(backslashed, ".git"), `gitdir: C:\work\tidedesk\.git\worktrees\wt-y`)
	relative := mkdir("scratch", "wt-z")
	write(filepath.Join(relative, ".git"), "gitdir: ../../jobpilot/.git/worktrees/wt-z\n")
	bare := mkdir("scratch", "wt-bare")
	write(filepath.Join(bare, ".git"), "gitdir: /srv/tools.git/worktrees/wt-bare\n")
	sub := mkdir("harness", "vendor", "lib")
	write(filepath.Join(sub, ".git"), "gitdir: ../../.git/modules/lib\n")
	plain := mkdir("notes")

	for dir, want := range map[string]string{
		main:        "harness",
		wt:          "harness",
		backslashed: "tidedesk",
		relative:    "jobpilot",
		bare:        "tools",
		sub:         "lib",
		plain:       "notes",
		filepath.Join(main, ".hrn", "slots", "3"): "harness",
		filepath.Join(root, "deleted-worktree"):   "deleted-worktree",
		"":                                        "",
	} {
		if got := repoName(dir); got != want {
			t.Errorf("repoName(%q) = %q, want %q", dir, got, want)
		}
	}
}

func TestTheCardSaysWhichRepositoriesTheRunsWereIn(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	root := t.TempDir()
	main := filepath.Join(root, "harness")
	wt := filepath.Join(root, "wt-review")
	for _, d := range []string{filepath.Join(main, ".git", "worktrees", "wt-review"), wt} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.ToSlash(filepath.Join(main, ".git", "worktrees", "wt-review"))), 0o644); err != nil {
		t.Fatal(err)
	}
	saveRunIn(t, main, "fix the login bug", agent.Result{CostUSD: 0.5})
	saveRunIn(t, wt, "review the diff", agent.Result{CostUSD: 0.25})
	saveRunIn(t, wt, "review the plan", agent.Result{CostUSD: 0.25})
	saveRunIn(t, filepath.Join(root, "jobpilot"), "add a cache", agent.Result{})

	c := cardOf(t)
	if r := repoRowOf(t, c, "harness"); r.Row.Runs != "3" || r.Row.Cost != "$1.0000" || r.Mix != "review 2 · debug 1" {
		t.Errorf("harness row = %+v", r)
	}
	if r := repoRowOf(t, c, "jobpilot"); r.Row.Runs != "1" || r.Row.Cost != "-" || r.Row.NoCost != "1 run reports no cost" || r.Mix != "implement 1" {
		t.Errorf("jobpilot row = %+v", r)
	}
	if len(c.Repos) != 2 || c.Repos[0].Row.Purpose != "harness" {
		t.Errorf("repository rows = %+v, want harness first, then jobpilot", c.Repos)
	}

	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	_, page := get(t, srv.URL+"/")
	for _, want := range []string{`<th scope="col">Repository</th>`, `<th scope="row">harness</th>`, `<td class="mix">review 2 · debug 1</td>`} {
		if !strings.Contains(page, want) {
			t.Errorf("the index lacks %q", want)
		}
	}
}

func TestSmallRepositoriesFoldIntoOneRow(t *testing.T) {
	var runs []tallied
	for i := range repoRows + 3 {
		for range repoRows + 3 - i {
			runs = append(runs, tallyOf(runlog.Record{Done: true, Task: agent.Task{Prompt: "fix the bug"}, Result: agent.Result{Backend: "claude", CostUSD: 0.01}}, fmt.Sprintf("repo%02d", i)))
		}
	}
	c := cardOver(runs)
	if len(c.Repos) != repoRows+1 {
		t.Fatalf("%d repository rows, want %d and one for the rest", len(c.Repos), repoRows)
	}
	if first := c.Repos[0]; first.Row.Purpose != "repo00" || first.Row.Runs != fmt.Sprint(repoRows+3) {
		t.Errorf("first row = %+v, want the repository with the most runs", first)
	}
	if other := c.Repos[repoRows]; other.Row.Purpose != "3 other folders" || other.Row.Runs != "6" || other.Mix != "debug 6" {
		t.Errorf("folded row = %+v", other)
	}

	// One repository past the limit is shown as itself, not folded into a row of one.
	var eleven []tallied
	for _, r := range runs {
		if r.repo <= fmt.Sprintf("repo%02d", repoRows) {
			eleven = append(eleven, r)
		}
	}
	c = cardOver(eleven)
	if len(c.Repos) != repoRows+1 || c.Repos[repoRows].Row.Purpose != fmt.Sprintf("repo%02d", repoRows) {
		t.Errorf("rows = %+v", c.Repos)
	}
}

func TestAFinishedRunIsReadOnceAndARunningOneEveryTime(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	saveRun(t, "claude", "fix the login bug", &agent.Result{CostUSD: 0.01})
	saveRun(t, "claude", "add a cache", nil)

	s := &server{}
	local := owner.LocalViewer()
	for range 3 {
		if c := cardOver(s.tallyRuns(local)); purposeRowOf(t, c, "all").Runs != "2" {
			t.Fatalf("card = %+v", c)
		}
	}
	if s.tally.loads != 4 {
		t.Errorf("%d records read over three refreshes, want 4: the finished one once, the running one each time", s.tally.loads)
	}
}

func TestAPrunedRunDropsOutOfTheCard(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	saveRun(t, "claude", "fix the login bug", &agent.Result{CostUSD: 0.01})
	gone := saveRun(t, "claude", "review the diff", &agent.Result{CostUSD: 0.01})

	s := &server{}
	local := owner.LocalViewer()
	if r := purposeRowOf(t, cardOver(s.tallyRuns(local)), "all"); r.Runs != "2" {
		t.Fatalf("all row = %+v", r)
	}
	if err := os.RemoveAll(filepath.Join(runlog.Root(), gone)); err != nil {
		t.Fatal(err)
	}
	c := cardOver(s.tallyRuns(local))
	if r := purposeRowOf(t, c, "all"); r.Runs != "1" || r.Cost != "$0.0100" {
		t.Errorf("all row after pruning = %+v", r)
	}
	if len(s.tally.m) != 1 {
		t.Errorf("the cache keeps %d runs, want 1", len(s.tally.m))
	}
}

func TestTheCardCountsOnlyRunsTheViewerMayRead(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	saveRun(t, "claude", "fix the login bug", &agent.Result{CostUSD: 0.01})

	s := &server{}
	if c := cardOver(s.tallyRuns(owner.Viewer{ID: owner.Email("visitor@example.com")})); c != nil {
		t.Errorf("a viewer who may read no run sees a card: %+v", c)
	}
	if c := cardOver(s.tallyRuns(owner.LocalViewer())); c == nil || purposeRowOf(t, c, "all").Runs != "1" {
		t.Errorf("the owner's card = %+v", c)
	}
}
