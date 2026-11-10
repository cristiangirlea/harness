package ws

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSlotBranchOptionsAreRefusedBeforeGitStarts(t *testing.T) {
	f := newFixture(t)
	trace := filepath.Join(t.TempDir(), "trace.json")
	t.Setenv("GIT_TRACE2_EVENT", filepath.ToSlash(trace))
	_, err := Take(context.Background(), TakeOptions{Repo: f.repo, Branch: "--force"})
	if err == nil || !strings.Contains(err.Error(), `"--force" is not a valid branch name`) {
		t.Errorf("Take = %v", err)
	}
	if data, err := os.ReadFile(trace); len(data) != 0 || (err != nil && !os.IsNotExist(err)) {
		t.Errorf("invalid branch started git: %s, %v", data, err)
	}
}

func TestInvalidSlotBranchNamesFailBeforePreparingSlots(t *testing.T) {
	for _, branch := range []string{"HEAD", "@{-1}"} {
		t.Run(branch, func(t *testing.T) {
			f := newFixture(t)
			git(t, f.repo, "switch", "-c", "other")
			git(t, f.repo, "switch", "main")
			_, err := Take(context.Background(), TakeOptions{Repo: f.repo, Branch: branch, NoFetch: true})
			if err == nil || err.Error() != `"`+branch+`" is not a valid branch name` {
				t.Errorf("Take = %v", err)
			}
			if _, err := os.Stat(SlotsDir(f.repo)); !os.IsNotExist(err) {
				t.Errorf("slots directory was prepared: %v", err)
			}
		})
	}
}

func TestSlotBranchNamesDoNotExpandCheckoutHistory(t *testing.T) {
	f := newFixture(t)
	git(t, f.repo, "switch", "-c", "other")
	git(t, f.repo, "switch", "main")
	if _, err := Take(context.Background(), TakeOptions{Repo: f.repo, Branch: "@{-1}", NoFetch: true}); err == nil {
		t.Fatal("Take accepted checkout history as a branch name")
	}
}

func TestSlotSwitchPlansEndOptionsBeforeTheirRevisions(t *testing.T) {
	f := newFixture(t)
	head := git(t, f.repo, "rev-parse", "HEAD")
	git(t, f.repo, "update-ref", "refs/heads/-base", head)
	for _, branch := range []string{"", "task"} {
		var notes []string
		args, err := planSwitch(context.Background(), f.repo, "-base", TakeOptions{Branch: branch, NoFetch: true}, &notes)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := gitOut(context.Background(), f.repo, args...); err != nil {
			t.Fatal(err)
		}
		if got := git(t, f.repo, "rev-parse", "HEAD"); got != head {
			t.Errorf("HEAD = %s, want %s", got, head)
		}
	}
}

func TestSlotCreationDoesNotTrackATagNamedLikeARemoteBranch(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	f := newFixture(t)
	base := git(t, f.repo, "rev-parse", "HEAD")
	f.commit(f.repo, "later.txt")
	git(t, f.repo, "tag", "refs/remotes/origin/task", "HEAD")
	got := f.take(TakeOptions{Branch: "task", Base: base, Holder: "test", NoFetch: true})
	if head := git(t, got.Path, "rev-parse", "HEAD"); head != base {
		t.Errorf("slot HEAD = %s, want %s", head, base)
	}
	if branch := git(t, got.Path, "symbolic-ref", "HEAD"); branch != "refs/heads/task" {
		t.Errorf("slot branch = %q", branch)
	}
}

func TestSlotTakeAndReleaseReadDashNamedBasesOnlyAsRevisions(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	f := newFixture(t)
	base := git(t, f.repo, "rev-parse", "HEAD")
	git(t, f.repo, "update-ref", "refs/heads/-base", base)
	got := f.take(TakeOptions{Branch: "task", Base: "-base", Holder: "test", NoFetch: true})
	if err := Release(context.Background(), got, "-base"); err != nil {
		t.Fatal(err)
	}
	if head := git(t, got.Path, "rev-parse", "HEAD"); head != base {
		t.Errorf("released HEAD = %s, want %s", head, base)
	}
}

func TestSlotTakeFetchesTheExactRemoteBranchDespiteATag(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	f := newFixture(t)
	base := git(t, f.repo, "rev-parse", "HEAD")
	head := f.elsewhere("task", "remote.txt")
	git(t, f.repo, "update-ref", "-d", "refs/remotes/origin/task")
	git(t, f.repo, "tag", "task", base)
	got := f.take(TakeOptions{Branch: "task", Holder: "test"})
	if commit := git(t, got.Path, "rev-parse", "HEAD"); commit != head {
		t.Errorf("slot HEAD = %s, want %s", commit, head)
	}
	if branch := git(t, got.Path, "symbolic-ref", "HEAD"); branch != "refs/heads/task" {
		t.Errorf("slot branch = %q", branch)
	}
}

func TestFreeResolvesTheMainCommitForAnUnbornSlotDespiteACollidingFile(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	f := newFixture(t)
	base := git(t, f.repo, "rev-parse", "HEAD")
	f.write(f.repo, base, "collision\n")
	got := f.take(TakeOptions{Branch: "task", Holder: "test", NoFetch: true})
	git(t, got.Path, "switch", "--orphan", "unborn")
	if _, err := Free(context.Background(), got.Path); err != nil {
		t.Fatal(err)
	}
	if head := git(t, got.Path, "rev-parse", "HEAD"); head != base {
		t.Errorf("freed HEAD = %s, want %s", head, base)
	}
}

func TestCommitResolutionReadsADashNamedRefOnlyAsARevision(t *testing.T) {
	f := newFixture(t)
	head := git(t, f.repo, "rev-parse", "HEAD")
	git(t, f.repo, "update-ref", "refs/heads/-base", head)
	got, err := commitOf(context.Background(), f.repo, "-base")
	if err != nil || got != head {
		t.Errorf("commitOf = %q, %v; want %s", got, err, head)
	}
}

func TestTakingAndReleasingASlotUsesTheBranchDespiteCollidingTagsAndFiles(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	f := newFixture(t)
	base := git(t, f.repo, "rev-parse", "HEAD")
	git(t, f.repo, "branch", "task", base)
	f.commit(f.repo, "later.txt")
	git(t, f.repo, "tag", "task", "HEAD")
	f.write(f.repo, "task", "colliding file\n")
	got := f.take(TakeOptions{Branch: "task", Base: base, Holder: "test", PID: os.Getpid(), NoFetch: true})
	if head := git(t, got.Path, "rev-parse", "HEAD"); head != base {
		t.Errorf("slot HEAD = %s, want branch tip %s", head, base)
	}
	if branch := git(t, got.Path, "symbolic-ref", "HEAD"); branch != "refs/heads/task" {
		t.Errorf("slot branch = %q", branch)
	}
	if err := Release(context.Background(), got, base); err != nil {
		t.Fatal(err)
	}
	if head := git(t, got.Path, "rev-parse", "HEAD"); head != base {
		t.Errorf("released HEAD = %s, want %s", head, base)
	}
	if branch := git(t, got.Path, "branch", "--show-current"); branch != "" {
		t.Errorf("released slot remains on branch %q", branch)
	}
}

func TestTakingASlotDoesNotMistakeATagForAnExistingBranch(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	f := newFixture(t)
	base := git(t, f.repo, "rev-parse", "HEAD")
	git(t, f.repo, "tag", "refs/heads/task", base)
	got := f.take(TakeOptions{Branch: "task", Base: base, Holder: "test", PID: os.Getpid(), NoFetch: true})
	if branch := git(t, got.Path, "symbolic-ref", "HEAD"); branch != "refs/heads/task" {
		t.Errorf("slot branch = %q", branch)
	}
	if head := git(t, got.Path, "rev-parse", "HEAD"); head != base {
		t.Errorf("slot HEAD = %s, want %s", head, base)
	}
	git(t, f.repo, "show-ref", "--verify", "refs/tags/refs/heads/task")
}

func TestSlotCreationAndFreeKeepRevisionsSeparateFromFilesAndTags(t *testing.T) {
	for _, mode := range []string{"detached", "new branch", "remote branch"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			f := newFixture(t)
			base := git(t, f.repo, "rev-parse", "HEAD")
			f.write(f.repo, base, "revision collision\n")
			git(t, f.repo, "tag", "task", base)
			opt := TakeOptions{Base: base, Holder: "test", NoFetch: true}
			if mode != "detached" {
				opt.Branch = "task"
			}
			if mode == "remote branch" {
				git(t, f.repo, "update-ref", "refs/remotes/origin/task", base)
			}
			got := f.take(opt)
			if head := git(t, got.Path, "rev-parse", "HEAD"); head != base {
				t.Errorf("slot HEAD = %s, want %s", head, base)
			}
			if branch := git(t, got.Path, "branch", "--show-current"); branch != opt.Branch {
				t.Errorf("slot branch = %q, want %q", branch, opt.Branch)
			}
			if _, err := Free(context.Background(), got.Path); err != nil {
				t.Fatal(err)
			}
			if branch := git(t, got.Path, "branch", "--show-current"); branch != "" {
				t.Errorf("freed slot is still on branch %q", branch)
			}
			if head := git(t, got.Path, "rev-parse", "HEAD"); head != base {
				t.Errorf("freed HEAD = %s, want %s", head, base)
			}
			git(t, f.repo, "show-ref", "--verify", "refs/tags/task")
		})
	}
}
