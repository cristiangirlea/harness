package eval

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
)

// snoopJudge records everything a judge could read where it runs: its prompt,
// its directory, the files there, and every ref and object of its repository.
type snoopJudge struct {
	mu   *sync.Mutex
	seen *[]snooped
}

type snooped struct {
	dir, readable string
	copies        int             // hrn-judge-* directories in the temp dir at the time
	alive         map[string]bool // which earlier judge dirs still existed
}

// snoopAfter, when set, runs after each judgement with the judge's dir; tests restore it.
var snoopAfter func(dir string)

func (snoopJudge) Name() string            { return "ev-judge-snoop" }
func (snoopJudge) Detect() (string, error) { return "stub", nil }
func (j snoopJudge) Run(_ context.Context, task agent.Task, _ func(agent.Event)) agent.Result {
	var b strings.Builder
	b.WriteString(task.Prompt + "\n" + task.Dir + "\n")
	for _, args := range [][]string{
		{"rev-parse", "--abbrev-ref", "HEAD"},
		{"status"},
		{"log", "--all", "--format=%H %D%n%B"},
		{"for-each-ref"},
		{"reflog", "--all"},
		{"worktree", "list"},
		{"cat-file", "--batch-all-objects", "--batch"},
	} {
		out, err := exec.Command("git", append([]string{"-C", task.Dir}, args...)...).CombinedOutput()
		if err != nil {
			b.WriteString("git " + strings.Join(args, " ") + " failed: " + err.Error() + "\n")
		}
		b.Write(out)
	}
	_ = filepath.WalkDir(task.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b.WriteString(path + "\n")
		if data, err := os.ReadFile(path); err == nil {
			b.Write(data)
		}
		return nil
	})
	entries, _ := os.ReadDir(os.TempDir())
	copies := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "hrn-judge-") {
			copies++
		}
	}
	j.mu.Lock()
	alive := map[string]bool{}
	for _, s := range *j.seen {
		if _, err := os.Stat(s.dir); err == nil {
			alive[s.dir] = true
		}
	}
	*j.seen = append(*j.seen, snooped{dir: task.Dir, readable: b.String(), copies: copies, alive: alive})
	after := snoopAfter
	j.mu.Unlock()
	if after != nil {
		after(task.Dir)
	}
	return agent.Result{Status: agent.StatusOK, Final: `{"score": 9, "reason": "r"}`}
}

var snoop = snoopJudge{mu: &sync.Mutex{}, seen: &[]snooped{}}

func init() { agent.Register(snoop) }

func TestTheJudgeCannotReadWhichVariantMadeTheCell(t *testing.T) {
	dir := initRepo(t)
	snoop.mu.Lock()
	*snoop.seen = nil
	snoop.mu.Unlock()
	spec := testSpec(dir)
	spec.Matrix = []Variant{{Name: "opus-secret", Backend: "ev-ok"}}
	spec.Judge = &Judge{Backend: "ev-judge-snoop", Rubric: "out.txt holds the prompt", Repeats: 2}
	rec, res, err := Run(context.Background(), spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Remove(res)

	snoop.mu.Lock()
	seen := slices.Clone(*snoop.seen)
	snoop.mu.Unlock()
	if len(seen) != 8 || len(rec.Judgements) != 8 {
		t.Fatalf("judge ran %d times for %d verdicts, want 2 tasks x 2 repeats x 2 judge repeats", len(seen), len(rec.Judgements))
	}
	worktrees := map[string]bool{}
	for _, tr := range res.Tasks {
		worktrees[filepath.Clean(tr.Worktree)] = true
	}
	dirs := map[string]bool{}
	for _, s := range seen {
		dirs[s.dir] = true
		if worktrees[filepath.Clean(s.dir)] {
			t.Errorf("the judge ran in the cell's own worktree %s", s.dir)
		}
		if !strings.Contains(s.readable, "+++ b/out.txt") || !strings.Contains(s.readable, "do alpha") && !strings.Contains(s.readable, "do beta") {
			t.Errorf("the judge must still see the change and its files:\n%s", s.readable)
		}
		if strings.Contains(s.readable, " failed: ") {
			t.Errorf("the judge's checkout is not a working repository:\n%s", s.readable)
		}
		names := []string{"opus-secret", "ev-ok", "hrn/"}
		for _, tr := range res.Tasks {
			names = append(names, tr.ID, tr.Branch)
		}
		for _, name := range names {
			if name != "" && strings.Contains(s.readable, name) {
				t.Errorf("the judge can read %q in what it was given or where it ran:\n%s", name, s.readable)
			}
		}
	}
	if len(dirs) != 4 {
		t.Errorf("judge dirs = %v, want one per cell, shared by its repeats", dirs)
	}
	for d := range dirs {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("the judge's checkout %s is still there after the eval: %v", d, err)
		}
	}
}

func TestJudgeOrderIsASeededShuffleNotCellByCell(t *testing.T) {
	order := judgeOrder(12, "b1234")
	if !slices.Equal(order, judgeOrder(12, "b1234")) {
		t.Error("the same seed must give the same order")
	}
	sorted := slices.Sorted(slices.Values(order))
	for i, n := range sorted {
		if n != i {
			t.Fatalf("order %v is not a permutation of 0..11", order)
		}
	}
	if slices.IsSorted(order) {
		t.Errorf("order %v judges cell by cell", order)
	}
	if slices.Equal(order, judgeOrder(12, "b5678")) {
		t.Errorf("two evals got the same order %v", order)
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commitFile commits name with body in repo and returns the commit.
func commitFile(t *testing.T, repo, name, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", name)
	gitIn(t, repo, "commit", "-q", "-m", "variant opus-secret wrote "+name)
	return gitIn(t, repo, "rev-parse", "HEAD")
}

func TestJudgeAllAsksInTheSeededOrder(t *testing.T) {
	dir := initRepo(t)
	spec := testSpec(dir)
	spec.Matrix = []Variant{{Name: "one", Backend: "ev-ok"}, {Name: "two", Backend: "ev-ok"}}
	spec.Parallel = 1
	spec.Judge = &Judge{Backend: "ev-judge-snoop", Rubric: "r", Repeats: 2}
	var asked []string
	_, res, err := Run(context.Background(), spec, func(e batch.Event) {
		if e.Kind == "judge" {
			asked = append(asked, e.Task+" "+strings.SplitN(e.Text, ":", 2)[0])
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Remove(res)
	var cells []string
	for _, tr := range res.Tasks {
		if tr.Status == batch.StatusPass {
			cells = append(cells, tr.ID)
		}
	}
	var want []string
	for _, n := range judgeOrder(len(cells)*2, res.ID) {
		want = append(want, fmt.Sprintf("%s judge %d/2", cells[n/2], n%2+1))
	}
	if len(cells) != 8 || !slices.Equal(asked, want) {
		t.Errorf("asked in order\n%v\nwant\n%v", asked, want)
	}
}

func TestBlindCheckoutHoldsTheWholeChangeAsItsLastCommit(t *testing.T) {
	repo := initRepo(t)
	base := gitIn(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("kept.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", ".gitignore")
	commitFile(t, repo, "big.txt", strings.Repeat("x", maxJudgeDiff+10))
	gitIn(t, repo, "rm", "-q", "README.md")
	if err := os.WriteFile(filepath.Join(repo, "kept.log"), []byte("tracked though ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-f", "kept.log")
	gitIn(t, repo, "commit", "-q", "-m", "more")
	commit := gitIn(t, repo, "rev-parse", "HEAD")

	dir, err := blindCheckout(context.Background(), repo, base, commit)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if got, want := gitIn(t, dir, "diff", "--stat", "HEAD~1", "HEAD"), gitIn(t, repo, "diff", "--stat", base, commit); got != want {
		t.Errorf("the checkout's last commit holds\n%s\nwant\n%s", got, want)
	}
	if got := gitIn(t, dir, "log", "--format=%s %an"); got != "candidate judge\nbase judge" {
		t.Errorf("history = %q", got)
	}
	if status := gitIn(t, dir, "status", "--porcelain", "--ignored"); status != "" {
		t.Errorf("the checkout is not clean:\n%s", status)
	}
}

func TestBlindCheckoutRunsNoProgramFromTheOperatorsGitConfig(t *testing.T) {
	repo := initRepo(t)
	base := gitIn(t, repo, "rev-parse", "HEAD")
	commit := commitFile(t, repo, "out.txt", "x\n")
	marker := filepath.Join(t.TempDir(), "ran")
	script := filepath.Join(t.TempDir(), "mark.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\npwd >> '"+filepath.ToSlash(marker)+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	hooks, templates := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(templates, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(hooks, "pre-commit"), filepath.Join(hooks, "post-commit"),
		filepath.Join(hooks, "post-checkout"), filepath.Join(templates, "hooks", "post-commit")} {
		data, _ := os.ReadFile(script)
		if err := os.WriteFile(path, data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	global := filepath.Join(t.TempDir(), "gitconfig")
	conf := "[core]\n\thooksPath = " + filepath.ToSlash(hooks) + "\n\tfsmonitor = " + filepath.ToSlash(script) +
		"\n[init]\n\ttemplateDir = " + filepath.ToSlash(templates) + "\n"
	if err := os.WriteFile(global, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)

	dir, err := blindCheckout(context.Background(), repo, base, commit)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	gitIn(t, dir, "status") // as the judge might, with the operator's config
	if data, err := os.ReadFile(marker); err == nil {
		t.Fatalf("a hook, template hook or fsmonitor from the global config ran in:\n%s", data)
	}
	gitIn(t, repo, "-c", "core.fsmonitor=false", "commit", "-q", "--allow-empty", "-m", "control")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control: the global hooks never run here, so the test proves nothing: %v", err)
	}
}

func TestTheJudgesGitIgnoresARepositoryNamedInTheEnvironment(t *testing.T) {
	fresh, other := t.TempDir(), t.TempDir()
	gitIn(t, fresh, "init", "-q")
	gitIn(t, other, "init", "-q")
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(other, ".git", "index"))
	if err := os.WriteFile(filepath.Join(fresh, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := blindGit(context.Background(), fresh, "add", "a.txt"); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"} {
		t.Setenv(v, "")
		os.Unsetenv(v)
	}
	if got := gitIn(t, fresh, "ls-files"); got != "a.txt" {
		t.Errorf("staged in the checkout: %q, want a.txt", got)
	}
	if got := gitIn(t, other, "ls-files"); got != "" {
		t.Errorf("staged in the repository GIT_DIR named: %q", got)
	}
}

func TestBlindCheckoutLeavesNothingBehindWhenItFails(t *testing.T) {
	repo := initRepo(t)
	tmp := t.TempDir()
	t.Setenv("TMP", tmp)
	t.Setenv("TMPDIR", tmp)
	base := gitIn(t, repo, "rev-parse", "HEAD")
	if dir, err := blindCheckout(context.Background(), repo, base, strings.Repeat("0", 40)); err == nil || dir != "" {
		t.Fatalf("checkout of a missing commit = %q, %v", dir, err)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("left behind in the temp dir: %v", left)
	}
}

// ownCommit commits its own work, so hrn finds a clean tree with the work already committed.
type ownCommit struct{}

func (ownCommit) Name() string            { return "ev-commits" }
func (ownCommit) Detect() (string, error) { return "stub", nil }
func (ownCommit) Run(_ context.Context, task agent.Task, _ func(agent.Event)) agent.Result {
	_ = os.WriteFile(filepath.Join(task.Dir, "out.txt"), []byte(task.Prompt), 0o644)
	for _, args := range [][]string{{"add", "out.txt"}, {"commit", "-q", "-m", "self"}} {
		if out, err := exec.Command("git", append([]string{"-C", task.Dir}, args...)...).CombinedOutput(); err != nil {
			return agent.Result{Status: agent.StatusError, Error: string(out)}
		}
	}
	return agent.Result{Status: agent.StatusOK, Final: "committed"}
}

func init() { agent.Register(ownCommit{}) }

func resetSnoop(t *testing.T) {
	t.Helper()
	snoop.mu.Lock()
	*snoop.seen = nil
	snoop.mu.Unlock()
	t.Cleanup(func() { snoopAfter = nil })
}

func seenBySnoop() []snooped {
	snoop.mu.Lock()
	defer snoop.mu.Unlock()
	return slices.Clone(*snoop.seen)
}

func TestTheJudgeSeesTheDiffOfAnAgentThatCommittedItsOwnWork(t *testing.T) {
	dir := initRepo(t)
	resetSnoop(t)
	spec := testSpec(dir)
	spec.Matrix = []Variant{{Backend: "ev-commits"}}
	spec.Judge = &Judge{Backend: "ev-judge-snoop", Rubric: "r"}
	rec, res, err := Run(context.Background(), spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Remove(res)
	seen := seenBySnoop()
	if len(seen) != 4 || len(rec.Judgements) != 4 {
		t.Fatalf("judge ran %d times, want one per cell", len(seen))
	}
	if row := rec.Table.Variants[0]; row.Passed != 4 || row.Unchanged != 0 || row.DidNotAct != 0 {
		t.Errorf("row = %+v: want every cell counted as changed and as having acted", row)
	}
	for _, s := range seen {
		prompt := s.readable[:strings.Index(s.readable, s.dir)]
		if !strings.Contains(prompt, "+++ b/out.txt") || strings.Contains(prompt, "(no changes)") {
			t.Errorf("the prompt misses the agent's own commit:\n%s", prompt)
		}
	}
}

func TestACellsCheckoutIsBuiltWhenFirstJudgedAndGoesAfterItsLastVerdict(t *testing.T) {
	dir := initRepo(t)
	resetSnoop(t)
	t.Setenv("TMP", t.TempDir())
	t.Setenv("TMPDIR", os.Getenv("TMP"))
	spec := testSpec(dir)
	spec.Matrix = []Variant{{Backend: "ev-ok"}}
	spec.Parallel = 1
	spec.Judge = &Judge{Backend: "ev-judge-snoop", Rubric: "r", Repeats: 2}
	_, res, err := Run(context.Background(), spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Remove(res)
	seen := seenBySnoop()
	if len(seen) != 8 {
		t.Fatalf("judge ran %d times, want 8", len(seen))
	}
	calls := map[string][]int{}
	for k, s := range seen {
		calls[s.dir] = append(calls[s.dir], k)
	}
	for k, s := range seen {
		started := 0
		for d, ks := range calls {
			if ks[0] <= k {
				started++
			}
			if len(ks) == 2 && ks[1] < k && s.alive[d] {
				t.Errorf("call %d: %s is still there after its last verdict", k, d)
			}
		}
		finished := 0
		for _, ks := range calls {
			if len(ks) == 2 && ks[1] < k {
				finished++
			}
		}
		if s.copies != started-finished {
			t.Errorf("call %d: %d copies on disk, want %d (built when first judged, gone after the last)", k, s.copies, started-finished)
		}
	}
}

func TestCancellingTheJudgeLeavesNoCheckoutBehind(t *testing.T) {
	dir := initRepo(t)
	resetSnoop(t)
	tmp := t.TempDir()
	t.Setenv("TMP", tmp)
	t.Setenv("TMPDIR", tmp)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	snoopAfter = func(string) { cancel() }
	spec := testSpec(dir)
	spec.Matrix = []Variant{{Backend: "ev-ok"}}
	spec.Parallel = 1
	spec.Judge = &Judge{Backend: "ev-judge-snoop", Rubric: "r", Repeats: 2}
	rec, res, _ := Run(ctx, spec, nil)
	defer batch.Remove(res)
	canceled := 0
	for _, j := range rec.Judgements {
		if j.Error == "canceled" {
			canceled++
		}
	}
	if len(seenBySnoop()) != 1 || len(rec.Judgements) != 8 || canceled != 7 {
		t.Errorf("judge ran %d times; verdicts %+v; want one run and the other 7 canceled", len(seenBySnoop()), rec.Judgements)
	}
	entries, _ := os.ReadDir(tmp)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "hrn-judge-") {
			t.Errorf("left behind after cancelling: %s", e.Name())
		}
	}
}

func TestAnEvidenceErrorAfterACancelIsRecordedAsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	failed := evidence{err: errors.New("commit abc is no longer in the repository")}
	if got := canceledOr(ctx, failed); got.err != failed.err {
		t.Errorf("before a cancel the error stays: %v", got.err)
	}
	cancel()
	if got := canceledOr(ctx, failed); got.err == nil || got.err.Error() != "canceled" {
		t.Errorf("after a cancel = %v, want canceled", got.err)
	}
	if got := canceledOr(ctx, evidence{dir: "d"}); got.err != nil {
		t.Errorf("built evidence gained an error: %v", got.err)
	}
}

func TestAFailedRemovalOfAJudgesCheckoutIsReported(t *testing.T) {
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		t.Skip("root can empty any directory")
	}
	dir := initRepo(t)
	resetSnoop(t)
	// Windows will not delete an open file; elsewhere a directory without write permission keeps its file.
	var locked []string
	var open []*os.File
	snoopAfter = func(dir string) {
		sub := filepath.Join(dir, "locked")
		if os.Mkdir(sub, 0o700) != nil || os.WriteFile(filepath.Join(sub, "f"), nil, 0o600) != nil {
			return
		}
		if runtime.GOOS == "windows" {
			if f, err := os.Open(filepath.Join(sub, "f")); err == nil {
				open = append(open, f)
				locked = append(locked, sub)
			}
		} else if os.Chmod(sub, 0o500) == nil {
			locked = append(locked, sub)
		}
	}
	t.Cleanup(func() {
		for _, f := range open {
			_ = f.Close()
		}
		for _, sub := range locked {
			_ = os.Chmod(sub, 0o700)
			_ = os.RemoveAll(filepath.Dir(sub))
		}
	})
	spec := testSpec(dir)
	spec.Matrix = []Variant{{Backend: "ev-ok"}}
	spec.Parallel = 1
	spec.Judge = &Judge{Backend: "ev-judge-snoop", Rubric: "r"}
	var reported []string
	_, res, err := Run(context.Background(), spec, func(e batch.Event) {
		if strings.Contains(e.Text, "leaving its checkout behind") {
			reported = append(reported, e.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Remove(res)
	if len(locked) != 4 || len(reported) != 4 {
		t.Errorf("locked %d checkouts, %d reported: %v", len(locked), len(reported), reported)
	}
}

func TestACheckoutLeftByACancelIsReportedOnce(t *testing.T) {
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		t.Skip("root can empty any directory")
	}
	dir := initRepo(t)
	resetSnoop(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var locked, judged string
	var open *os.File
	snoopAfter = func(dir string) {
		judged = dir
		sub := filepath.Join(dir, "locked")
		if os.Mkdir(sub, 0o700) == nil && os.WriteFile(filepath.Join(sub, "f"), nil, 0o600) == nil {
			if runtime.GOOS == "windows" {
				open, _ = os.Open(filepath.Join(sub, "f"))
			} else {
				_ = os.Chmod(sub, 0o500)
			}
			locked = sub
		}
		cancel()
	}
	t.Cleanup(func() {
		if open != nil {
			_ = open.Close()
		}
		if locked != "" {
			_ = os.Chmod(locked, 0o700)
			_ = os.RemoveAll(filepath.Dir(locked))
		}
	})
	spec := testSpec(dir)
	spec.Matrix = []Variant{{Backend: "ev-ok"}}
	spec.Parallel = 1
	spec.Judge = &Judge{Backend: "ev-judge-snoop", Rubric: "r", Repeats: 2}
	var reported []string
	_, res, _ := Run(ctx, spec, func(e batch.Event) {
		if strings.Contains(e.Text, "leaving its checkout behind") {
			reported = append(reported, e.Text)
		}
	})
	defer batch.Remove(res)
	if locked == "" || len(reported) != 1 || !strings.Contains(reported[0], filepath.Base(judged)) {
		t.Errorf("locked %q; reports %v; want one naming the cancelled cell's checkout", locked, reported)
	}
}
