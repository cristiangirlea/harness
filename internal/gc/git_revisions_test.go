package gc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPatchLogReadsADashNamedRangeDespiteACollidingFile(t *testing.T) {
	f := newFixture(t)
	base := git(t, f.repo, "rev-parse", "HEAD")
	_, head := f.worktree("task", "hrn/task")
	git(t, f.repo, "update-ref", "refs/heads/-base", base)
	f.write(f.repo, "-base.."+head, "collision\n")
	out, err := logPatchIDs(context.Background(), f.repo, "-base", head)
	if err != nil || !strings.Contains(out, head) {
		t.Errorf("logPatchIDs = %q, %v; want %s", out, err, head)
	}
	if ids := landedPatchIDs(context.Background(), f.repo, "-base", head); len(ids) != 1 || ids[0].commit != head {
		t.Errorf("landedPatchIDs = %v; want %s", ids, head)
	}
}

func TestAncestryReadsBothWorktreeRevisionsOnlyAsRevisions(t *testing.T) {
	f := newFixture(t)
	base := git(t, f.repo, "rev-parse", "HEAD")
	_, head := f.worktree("task", "hrn/task")
	git(t, f.repo, "update-ref", "refs/heads/-base", base)
	git(t, f.repo, "update-ref", "refs/heads/-head", head)
	for _, pair := range [][2]string{{"-base", head}, {base, "-head"}, {"-base", "-head"}} {
		if !isAncestor(context.Background(), f.repo, pair[0], pair[1]) {
			t.Errorf("isAncestor(%q, %q) did not recognize the base", pair[0], pair[1])
		}
		if isAncestor(context.Background(), f.repo, pair[1], pair[0]) {
			t.Errorf("isAncestor(%q, %q) reversed ancestry", pair[1], pair[0])
		}
	}
	for _, pair := range [][2]string{{"--all", head}, {base, "--all"}, {"", head}, {base, ""}} {
		if isAncestor(context.Background(), f.repo, pair[0], pair[1]) {
			t.Errorf("isAncestor(%q, %q) accepted a non-revision", pair[0], pair[1])
		}
	}
}

func TestMergeEquivalenceReadsRevisionsDespiteCollidingFilesAndDashNames(t *testing.T) {
	for _, collision := range []string{"files", "dash head", "dash default"} {
		t.Run(collision, func(t *testing.T) {
			f := newFixture(t)
			base := git(t, f.repo, "rev-parse", "HEAD")
			_, head := f.worktree("task", "hrn/task")
			git(t, f.repo, "merge", "--squash", "refs/heads/hrn/task")
			git(t, f.repo, "commit", "-q", "-m", "landed task")
			def := git(t, f.repo, "rev-parse", "HEAD")
			switch collision {
			case "files":
				for _, revision := range []string{base, head, def} {
					f.write(f.repo, revision, "revision collision\n")
				}
			case "dash head":
				git(t, f.repo, "update-ref", "refs/heads/-task", head)
				head = "-task"
			case "dash default":
				git(t, f.repo, "update-ref", "refs/heads/-default", def)
				def = "-default"
			}
			facts := &repoFacts{repo: f.repo, def: def}
			if !mergeChangesNothing(context.Background(), facts, head) {
				t.Error("already-landed change was not recognized")
			}
		})
	}
}

func TestLandedChangesReadDashNamedWorktreeAndDefaultRevisions(t *testing.T) {
	for _, which := range []string{"head", "default"} {
		t.Run(which, func(t *testing.T) {
			f := newFixture(t)
			_, head := f.worktree("task", "hrn/task")
			git(t, f.repo, "merge", "--squash", "refs/heads/hrn/task")
			git(t, f.repo, "commit", "-q", "-m", "landed task")
			landed := git(t, f.repo, "rev-parse", "HEAD")
			def := landed
			if which == "head" {
				git(t, f.repo, "update-ref", "refs/heads/-task", head)
				head = "-task"
			} else {
				git(t, f.repo, "update-ref", "refs/heads/-default", def)
				def = "-default"
			}
			if got := landedBatchChange(context.Background(), &repoFacts{repo: f.repo, def: def}, head); got != landed {
				t.Errorf("landedBatchChange = %q, want %s", got, landed)
			}
		})
	}
}

func TestBranchDeletionRefusesInvalidRecordedNamesBeforeGitStarts(t *testing.T) {
	for _, branch := range []string{"", "--force"} {
		t.Run(branch, func(t *testing.T) {
			f := newFixture(t)
			head := git(t, f.repo, "rev-parse", "HEAD")
			trace := filepath.Join(t.TempDir(), "git-trace.json")
			t.Setenv("GIT_TRACE2_EVENT", filepath.ToSlash(trace))
			err := deleteBranch(context.Background(), f.repo, branch, head)
			if err == nil || !strings.Contains(err.Error(), "invalid recorded branch") || !strings.Contains(err.Error(), fmt.Sprintf("%q", branch)) {
				t.Errorf("deleteBranch = %v; want an error naming %q", err, branch)
			}
			data, readErr := os.ReadFile(trace)
			if readErr != nil && !os.IsNotExist(readErr) {
				t.Fatal(readErr)
			}
			if len(data) != 0 {
				t.Errorf("invalid branch started git:\n%s", data)
			}
		})
	}
}

func TestBranchDeletionUsesTheExactBranchAndConfirmedCommit(t *testing.T) {
	f := newFixture(t)
	base := git(t, f.repo, "rev-parse", "HEAD")
	head := git(t, f.repo, "commit-tree", base+"^{tree}", "-p", base, "-m", "task")
	git(t, f.repo, "branch", "task", head)
	git(t, f.repo, "tag", "task", base)
	if err := deleteBranch(context.Background(), f.repo, "task", base); err == nil {
		t.Error("deleted a branch whose tip moved")
	}
	if got := git(t, f.repo, "rev-parse", "refs/heads/task"); got != head {
		t.Fatalf("task branch = %s, want %s", got, head)
	}
	if err := deleteBranch(context.Background(), f.repo, "task", head); err != nil {
		t.Fatal(err)
	}
	if got := git(t, f.repo, "for-each-ref", "--format=%(refname)", "refs/heads/task"); got != "" {
		t.Errorf("deleted branch still exists: %s", got)
	}
	git(t, f.repo, "show-ref", "--verify", "refs/tags/task")
}
