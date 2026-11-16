package batch

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/followup"
)

var hideNames = []string{"pkg/a_test.go", "scripts/check.sh"}

// peekStub writes what it could see of the protected repo's two files, then does what its prompt's first word says.
func peekStub(name string) stub {
	return stub{name, func(_ context.Context, task agent.Task) agent.Result {
		failed := func(err error) agent.Result {
			return agent.Result{Backend: name, Status: agent.StatusError, Error: "stub setup: " + err.Error(), Turns: 1}
		}
		var seen []string
		for _, f := range hideNames {
			if _, err := os.Stat(filepath.Join(task.Dir, filepath.FromSlash(f))); err == nil {
				seen = append(seen, f)
			}
		}
		if err := os.WriteFile(filepath.Join(task.Dir, "seen-by-"+name+".txt"), []byte("saw: "+strings.Join(seen, " ")+"\n"), 0o644); err != nil {
			return failed(err)
		}
		test := filepath.Join(task.Dir, "pkg", "a_test.go")
		switch strings.Fields(typed(task))[0] {
		case "plant":
			if err := os.WriteFile(test, []byte("package pkg\n// planted\n"), 0o644); err != nil {
				return failed(err)
			}
		case "folder":
			// A folder where the hidden file was, with something in it.
			if err := os.MkdirAll(test, 0o755); err != nil {
				return failed(err)
			}
			if err := os.WriteFile(filepath.Join(test, "inner.txt"), []byte("x\n"), 0o644); err != nil {
				return failed(err)
			}
		case "block":
			// A file where the hidden file's folder was, so it cannot be written back.
			if err := os.RemoveAll(filepath.Join(task.Dir, "pkg")); err != nil {
				return failed(err)
			}
			if err := os.WriteFile(filepath.Join(task.Dir, "pkg"), []byte("in the way\n"), 0o644); err != nil {
				return failed(err)
			}
		case "fail":
			return agent.Result{Backend: name, Status: agent.StatusError, Error: "model exploded", Turns: 1}
		case "limit":
			return agent.Result{Backend: name, Status: agent.StatusLimited, Error: "You've hit your usage limit.", LimitResets: time.Now().Add(time.Hour)}
		}
		return agent.Result{Backend: name, Status: agent.StatusOK, Final: "done", Turns: 1}
	}}
}

func init() { agent.Register(peekStub("wb-hidden")) }

// Both files must hold their own text again when a gate runs.
const filesAreBack = `test "$(cat pkg/a_test.go)" = "package pkg" && test "$(cat scripts/check.sh)" = "exit 0"`

func seenBy(t *testing.T, dir string, tr TaskResult, backend string) string {
	t.Helper()
	return strings.TrimSpace(git(t, dir, "show", tr.Branch+":seen-by-"+backend+".txt"))
}

func TestHiddenFilesAreGoneForTheAgentAndBackForTheGates(t *testing.T) {
	dir := protectedRepo(t)
	res, events := run(t, Spec{Dir: dir, Backend: "wb-hidden", Worktrees: WorktreesFresh, Hide: []string{"**/*_test.go", "scripts/**", "nothing/**"},
		Gate: []string{filesAreBack}, Tasks: []Task{{ID: "t", Prompt: "look"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusPass || len(tr.Attempts) != 1 || tr.Attempts[0].Hidden != 2 {
		t.Fatalf("task: %+v", tr)
	}
	if got := seenBy(t, dir, tr, "wb-hidden"); got != "saw:" {
		t.Fatalf("the agent could see hidden files: %q", got)
	}
	if out := git(t, dir, "diff", "--name-status", res.Base, tr.Branch); strings.TrimSpace(out) != "A\tseen-by-wb-hidden.txt" {
		t.Fatalf("the task's commit touches the hidden files: %q", out)
	}
	if s := eventTexts(events, "info"); !strings.Contains(s, "hid 2 files from the agent") {
		t.Errorf("info events = %q", s)
	}
}

func TestWithoutAHideListTheAgentSeesEveryFile(t *testing.T) {
	dir := protectedRepo(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-hidden", Worktrees: WorktreesFresh, Tasks: []Task{{ID: "t", Prompt: "look"}}})
	tr := res.Tasks[0]
	if got := seenBy(t, dir, tr, "wb-hidden"); got != "saw: pkg/a_test.go scripts/check.sh" || tr.Attempts[0].Hidden != 0 {
		t.Fatalf("saw %q, hidden %d", got, tr.Attempts[0].Hidden)
	}
}

func TestAHideListThatMatchesNothingHidesNothingAndSaysSo(t *testing.T) {
	dir := protectedRepo(t)
	res, events := run(t, Spec{Dir: dir, Backend: "wb-hidden", Worktrees: WorktreesFresh, Hide: []string{"nothing/**"},
		Gate: []string{filesAreBack}, Tasks: []Task{{ID: "t", Prompt: "look"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusPass || tr.Attempts[0].Hidden != 0 {
		t.Fatalf("task: %+v", tr)
	}
	if got := seenBy(t, dir, tr, "wb-hidden"); got != "saw: pkg/a_test.go scripts/check.sh" {
		t.Fatalf("saw %q", got)
	}
	if s := eventTexts(events, "info"); !strings.Contains(s, "hid 0 files from the agent") {
		t.Errorf("info events = %q", s)
	}
}

func TestMoreThanTheHideLimitIsRefusedBeforeAnythingIsRemoved(t *testing.T) {
	dir := protectedRepo(t)
	prev := hideMaxBytes
	hideMaxBytes = 15 // the first file fits and the second does not, so a remove-as-you-read would lose the first
	t.Cleanup(func() { hideMaxBytes = prev })
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-hidden", Worktrees: WorktreesFresh, Options: agent.Options{NoSwitch: true},
		Hide: []string{"**/*_test.go", "scripts/**"}, Retry: []string{"wb-hidden"}, Tasks: []Task{{ID: "t", Prompt: "look"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusError || len(tr.Attempts) != 1 || tr.Attempts[0].Hidden != 0 || tr.Commit != "" {
		t.Fatalf("task: %+v", tr)
	}
	if !strings.Contains(tr.Error, "hide: the matching files hold more than the 15 bytes") || tr.Attempts[0].Result.Turns != 0 {
		t.Fatalf("task error %q, turns %d: the agent must not have started", tr.Error, tr.Attempts[0].Result.Turns)
	}
	for _, name := range hideNames {
		if _, err := os.Stat(filepath.Join(tr.Worktree, filepath.FromSlash(name))); err != nil {
			t.Errorf("%s was removed although hiding was refused: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(tr.Worktree, "seen-by-wb-hidden.txt")); !os.IsNotExist(err) {
		t.Errorf("the agent ran: %v", err)
	}
}

func TestATasksHideListReplacesTheSpecs(t *testing.T) {
	dir := protectedRepo(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-hidden", Worktrees: WorktreesFresh, Hide: []string{"**/*_test.go", "scripts/**"},
		Tasks: []Task{{ID: "t", Prompt: "look", Hide: []string{"scripts/check.sh"}}}})
	tr := res.Tasks[0]
	if got := seenBy(t, dir, tr, "wb-hidden"); got != "saw: pkg/a_test.go" || tr.Attempts[0].Hidden != 1 {
		t.Fatalf("saw %q, hidden %d", got, tr.Attempts[0].Hidden)
	}
}

func TestWhatTheAgentPutAtAHiddenPathIsReplacedByTheHiddenFile(t *testing.T) {
	for _, what := range []string{"plant", "folder"} {
		t.Run(what, func(t *testing.T) {
			dir := protectedRepo(t)
			res, _ := run(t, Spec{Dir: dir, Backend: "wb-hidden", Worktrees: WorktreesFresh, Hide: []string{"**/*_test.go"},
				Protect: []string{"**/*_test.go"}, ProtectMode: ProtectFail, Gate: []string{filesAreBack}, Tasks: []Task{{ID: "t", Prompt: what}}})
			tr := res.Tasks[0]
			if tr.Status != StatusPass || len(tr.Attempts[0].Protected) != 0 {
				t.Fatalf("task: %+v", tr)
			}
			if out := git(t, dir, "diff", "--name-status", res.Base, tr.Branch); strings.TrimSpace(out) != "A\tseen-by-wb-hidden.txt" {
				t.Fatalf("what the agent put there survived: %q", out)
			}
		})
	}
}

func TestAFailedRunStillGetsItsHiddenFilesBack(t *testing.T) {
	dir := protectedRepo(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-hidden", Worktrees: WorktreesFresh, Options: agent.Options{NoSwitch: true},
		Hide: []string{"**/*_test.go", "scripts/**"}, Tasks: []Task{{ID: "t", Prompt: "fail"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusFail || tr.Attempts[0].Hidden != 2 {
		t.Fatalf("task: %+v", tr)
	}
	if tr.Commit == "" {
		t.Fatalf("the failed attempt's work was not committed, so the test proves nothing: %+v", tr)
	}
	if out := git(t, dir, "diff", "--name-status", res.Base, tr.Branch); strings.TrimSpace(out) != "A\tseen-by-wb-hidden.txt" {
		t.Fatalf("the failed attempt's commit touches hidden files: %q", out)
	}
}

func TestARetryInTheSameWorktreeHidesTheFilesAgain(t *testing.T) {
	dir := protectedRepo(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-hidden", Worktrees: WorktreesFresh, Options: agent.Options{NoSwitch: true},
		Hide: []string{"**/*_test.go", "scripts/**"}, Gate: []string{filesAreBack, "exit 1"}, Retry: []string{"wb-hidden"},
		Tasks: []Task{{ID: "t", Prompt: "look"}}})
	tr := res.Tasks[0]
	if len(tr.Attempts) != 2 || tr.Attempts[0].Hidden != 2 || tr.Attempts[1].Hidden != 2 || !tr.Attempts[1].Continued {
		t.Fatalf("attempts: %+v", tr.Attempts)
	}
	for i, a := range tr.Attempts {
		if len(a.Gates) != 2 || a.Gates[0].ExitCode != 0 {
			t.Errorf("attempt %d: the files were not back for its gates: %+v", i+1, a.Gates)
		}
	}
	if got := seenBy(t, dir, tr, "wb-hidden"); got != "saw:" {
		t.Fatalf("the retry could see hidden files: %q", got)
	}
}

func TestACarryOnAfterALimitHidesTheFilesFromThePartnerToo(t *testing.T) {
	dir := protectedRepo(t)
	for _, name := range append(followup.MeteredEnv("claude"), followup.MeteredEnv("codex")...) {
		t.Setenv(name, "")
	}
	for _, name := range []string{"codex", "claude"} {
		orig, err := agent.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		agent.Register(peekStub(name))
		t.Cleanup(func() { agent.Register(orig) })
	}
	// codex reads "limit" and stops at its limit; claude carries on with the same words and ends ok.
	limited := peekStub("codex")
	agent.Register(stub{"claude", func(ctx context.Context, task agent.Task) agent.Result {
		task.Prompt = "look"
		res := limited.run(ctx, task)
		res.Backend = "claude"
		return res
	}})
	res, _ := run(t, Spec{Dir: dir, Backend: "codex", Hide: []string{"**/*_test.go", "scripts/**"}, Gate: []string{filesAreBack},
		Tasks: []Task{{ID: "t", Prompt: "limit"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusPass || len(tr.Attempts) != 2 || tr.Attempts[1].SwitchedFrom != "codex" {
		t.Fatalf("task: %+v", tr)
	}
	if tr.Attempts[0].Hidden != 2 || tr.Attempts[1].Hidden != 2 {
		t.Fatalf("hidden: %d then %d", tr.Attempts[0].Hidden, tr.Attempts[1].Hidden)
	}
	// Both stubs write the same file name, so the carry-on's view is the one committed last.
	if got := seenBy(t, dir, tr, "codex"); got != "saw:" {
		t.Fatalf("an attempt could see hidden files: %q", got)
	}
}

func TestWhenTheHiddenFilesCannotGoBackNothingIsCommittedGatedOrRetried(t *testing.T) {
	dir := protectedRepo(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-hidden", Worktrees: WorktreesFresh, Options: agent.Options{NoSwitch: true},
		Hide: []string{"**/*_test.go"}, Gate: []string{"exit 0"}, Retry: []string{"wb-hidden"}, Tasks: []Task{{ID: "t", Prompt: "block"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusError || len(tr.Attempts) != 1 || len(tr.Attempts[0].Gates) != 0 || tr.Commit != "" {
		t.Fatalf("task: %+v", tr)
	}
	if !strings.Contains(tr.Error, "hide: putting the hidden files back") || !strings.Contains(tr.Attempts[0].Error, "hide: ") {
		t.Fatalf("task error %q, attempt error %q", tr.Error, tr.Attempts[0].Error)
	}
	if _, err := os.Stat(tr.Worktree); err != nil {
		t.Fatalf("the worktree was not kept for a person to look at: %v", err)
	}
	if out := git(t, dir, "rev-parse", tr.Branch); strings.TrimSpace(out) != res.Base {
		t.Fatalf("the branch moved off the base: %s", out)
	}
}

func TestHidingKeepsTheBytesGitCheckedOutWhateverTheAgentTellsGit(t *testing.T) {
	repo := protectedRepo(t)
	base := git(t, repo, "rev-parse", "HEAD")
	// A worktree git checked out, as a task's is.
	dir := filepath.Join(t.TempDir(), "wt")
	git(t, repo, "worktree", "add", "-q", "--detach", dir, base)
	before := map[string]string{}
	for _, name := range hideNames {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = string(data)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h, err := hideFiles(ctx, dir, base, []string{"**/*_test.go", "scripts/**", "nothing/**"})
	if err != nil || h.count() != 2 {
		t.Fatalf("hidden %d, %v", h.count(), err)
	}
	for _, name := range hideNames {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name))); !os.IsNotExist(err) {
			t.Fatalf("%s is still there: %v", name, err)
		}
	}
	// What an agent could do to change a checkout: attributes, sparse patterns, and a canceled batch.
	if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("* text eol=crlf working-tree-encoding=UTF-16\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "sparse-checkout", "set", "--no-cone", "/*", "!/pkg/", "!/scripts/")
	cancel()
	if err := h.restore(); err != nil {
		t.Fatal(err)
	}
	for _, name := range hideNames {
		if got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name))); err != nil || string(got) != before[name] {
			t.Errorf("%s: %q, %v, want %q", name, got, err, before[name])
		}
	}
}

func TestAMatchingFileThatIsAlreadyGoneIsNotCounted(t *testing.T) {
	dir := protectedRepo(t)
	base := git(t, dir, "rev-parse", "HEAD")
	if err := os.Remove(filepath.Join(dir, "scripts", "check.sh")); err != nil {
		t.Fatal(err)
	}
	h, err := hideFiles(context.Background(), dir, base, []string{"**/*_test.go", "scripts/**"})
	if err != nil || h.count() != 1 {
		t.Fatalf("hidden %d, %v", h.count(), err)
	}
	if err := h.restore(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "scripts", "check.sh")); !os.IsNotExist(err) {
		t.Fatalf("a file that was not there came back: %v", err)
	}
}

func TestRestoringNeverWritesThroughAHardLinkPlantedAfterThePathWasCleared(t *testing.T) {
	dir := protectedRepo(t)
	base := git(t, dir, "rev-parse", "HEAD")
	outside := filepath.Join(t.TempDir(), "theirs.txt")
	if err := os.WriteFile(outside, []byte("someone else's"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := hideFiles(context.Background(), dir, base, []string{"**/*_test.go"})
	if err != nil || h.count() != 1 {
		t.Fatalf("hidden %d, %v", h.count(), err)
	}
	defer h.root.Close()
	if err := h.clear(h.files[0]); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(dir, "pkg", "a_test.go")); err != nil {
		t.Skipf("no hard links between these folders: %v", err)
	}
	if err := h.create(h.files[0]); err == nil {
		t.Error("the file was written although something had reappeared at its path")
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "someone else's" {
		t.Fatalf("the file outside the worktree was written through the link: %q, %v", got, err)
	}
	// A restore that starts over clears the link, which leaves the other file whole.
	if err := h.restore(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "someone else's" {
		t.Fatalf("the file outside the worktree changed in the restore: %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "pkg", "a_test.go")); err != nil || !strings.HasPrefix(string(got), "package pkg") {
		t.Fatalf("the hidden file is not back: %q, %v", got, err)
	}
}

// link makes a symlink or skips where Windows withholds the privilege; anywhere else a failure is a failure.
func link(t *testing.T, target, name string) {
	t.Helper()
	if err := os.Symlink(target, name); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks need a privilege here: %v", err)
		}
		t.Fatal(err)
	}
}

func TestHidingNeverTouchesAFileOutsideTheWorktree(t *testing.T) {
	outsideFile := func(t *testing.T) (string, func()) {
		outside := t.TempDir()
		for _, name := range []string{"a_test.go", filepath.Join("pkg", "a_test.go")} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(outside, name)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(outside, name), []byte("someone else's\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return outside, func() {
			t.Helper()
			for _, name := range []string{"a_test.go", filepath.Join("pkg", "a_test.go")} {
				if got, err := os.ReadFile(filepath.Join(outside, name)); err != nil || string(got) != "someone else's\n" {
					t.Fatalf("%s outside the worktree was touched: %q, %v", name, got, err)
				}
			}
		}
	}
	t.Run("a folder inside swapped for a link before hiding", func(t *testing.T) {
		dir := protectedRepo(t)
		base := git(t, dir, "rev-parse", "HEAD")
		outside, untouched := outsideFile(t)
		if err := os.RemoveAll(filepath.Join(dir, "pkg")); err != nil {
			t.Fatal(err)
		}
		link(t, outside, filepath.Join(dir, "pkg"))
		h, err := hideFiles(context.Background(), dir, base, []string{"**/*_test.go"})
		if err == nil {
			t.Error("hiding through a link out of the worktree did not fail")
		}
		_ = h.restore()
		untouched()
	})
	t.Run("a folder inside swapped for a link while the agent ran", func(t *testing.T) {
		dir := protectedRepo(t)
		base := git(t, dir, "rev-parse", "HEAD")
		outside, untouched := outsideFile(t)
		h, err := hideFiles(context.Background(), dir, base, []string{"**/*_test.go"})
		if err != nil || h.count() != 1 {
			t.Fatalf("hidden %d, %v", h.count(), err)
		}
		defer h.root.Close() // a skip below must not leave the folder open
		if err := os.RemoveAll(filepath.Join(dir, "pkg")); err != nil {
			t.Fatal(err)
		}
		link(t, outside, filepath.Join(dir, "pkg"))
		if err := h.restore(); err == nil {
			t.Error("restoring through a link out of the worktree did not fail")
		}
		untouched()
	})
	t.Run("the worktree itself swapped for a link while the agent ran", func(t *testing.T) {
		dir := protectedRepo(t)
		base := git(t, dir, "rev-parse", "HEAD")
		outside, untouched := outsideFile(t)
		h, err := hideFiles(context.Background(), dir, base, []string{"**/*_test.go"})
		if err != nil || h.count() != 1 {
			t.Fatalf("hidden %d, %v", h.count(), err)
		}
		defer h.root.Close() // a skip or a failure below must not leave the folder open
		moved := dir + "-moved"
		if err := os.Rename(dir, moved); err != nil {
			if runtime.GOOS == "windows" {
				t.Skipf("an open folder cannot be renamed here: %v", err)
			}
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(dir); _ = os.Rename(moved, dir) })
		link(t, outside, dir)
		if err := h.restore(); err != nil {
			t.Fatal(err)
		}
		// The handle still is the real worktree, wherever its name went.
		if got, err := os.ReadFile(filepath.Join(moved, "pkg", "a_test.go")); err != nil || !strings.HasPrefix(string(got), "package pkg") {
			t.Errorf("the file did not go back into the worktree: %q, %v", got, err)
		}
		untouched()
	})
	t.Run("the worktree itself swapped for a link", func(t *testing.T) {
		dir := protectedRepo(t)
		base := git(t, dir, "rev-parse", "HEAD")
		outside, untouched := outsideFile(t)
		moved := dir + "-moved"
		if err := os.Rename(dir, moved); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(dir); _ = os.Rename(moved, dir) })
		link(t, outside, dir)
		h, err := hideFiles(context.Background(), dir, base, []string{"**/*_test.go"})
		if err == nil || !strings.Contains(err.Error(), "not a plain folder") {
			t.Errorf("err = %v, want a refusal of the linked worktree", err)
		}
		_ = h.restore()
		untouched()
	})
}

func TestABadHideGlobIsRefusedBeforeAnyTaskRuns(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec Spec
		want string
	}{
		{"spec", Spec{Hide: []string{"../secrets"}, Tasks: []Task{{ID: "t", Prompt: "look"}}}, `hide "../secrets"`},
		{"task", Spec{Tasks: []Task{{ID: "t", Prompt: "look", Hide: []string{`a\b`}}}}, `task t: hide "a\\b"`},
		{"pattern", Spec{Hide: []string{"[z"}, Tasks: []Task{{ID: "t", Prompt: "look"}}}, `hide "[z"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.spec.Dir, tc.spec.Backend = protectedRepo(t), "wb-hidden"
			_, err := Run(context.Background(), tc.spec, func(Event) {})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to name %s", err, tc.want)
			}
		})
	}
}
