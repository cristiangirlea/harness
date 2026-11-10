package batch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentBranchHelpersRefuseOptionsBeforeGitStarts(t *testing.T) {
	for _, operation := range []string{"tips", "commit", "agent commit"} {
		t.Run(operation, func(t *testing.T) {
			dir := initRepo(t)
			base := git(t, dir, "rev-parse", "HEAD")
			check := forbidRevisionGit(t)
			var err error
			switch operation {
			case "tips":
				_, err = gitTips(dir, "--force")
			case "commit":
				_, _, err = commitChanges(dir, "--force", "task")
			case "agent commit":
				_, _, err = agentCommit(dir, "--force", base)
			}
			check()
			if err == nil || !strings.Contains(err.Error(), `invalid recorded branch "--force"`) {
				t.Errorf("%s = %v", operation, err)
			}
		})
	}
}

func TestMergeMessagesAndReflogsMatchGitWithOtherAuthors(t *testing.T) {
	for _, comment := range []string{"#", ";"} {
		for _, squash := range []bool{false, true} {
			for _, action := range []string{"unset", "", "caller action"} {
				t.Run(fmt.Sprintf("%s/squash=%v/action=%s", comment, squash, action), func(t *testing.T) {
					dir := initRepo(t)
					git(t, dir, "config", "merge.log", "true")
					git(t, dir, "config", "core.commentChar", comment)
					git(t, dir, "config", "core.commentString", comment+comment)
					git(t, dir, "switch", "-q", "-c", "hrn/task")
					writeFile(t, filepath.Join(dir, "task.txt"), "task work\n")
					git(t, dir, "add", "task.txt")
					t.Run("other author", func(t *testing.T) {
						t.Setenv("GIT_AUTHOR_NAME", "Other Author")
						t.Setenv("GIT_AUTHOR_EMAIL", "author@example.com")
						t.Setenv("GIT_COMMITTER_NAME", "Other Committer")
						t.Setenv("GIT_COMMITTER_EMAIL", "committer@example.com")
						git(t, dir, "commit", "-q", "-m", "task work")
					})
					head := git(t, dir, "rev-parse", "HEAD")
					git(t, dir, "switch", "-q", "main")
					twin := t.TempDir()
					if err := os.CopyFS(twin, os.DirFS(dir)); err != nil {
						t.Fatal(err)
					}
					t.Setenv("GIT_REFLOG_ACTION", action)
					if action == "unset" {
						if err := os.Unsetenv("GIT_REFLOG_ACTION"); err != nil {
							t.Fatal(err)
						}
					}
					if squash {
						git(t, twin, "merge", "--squash", "hrn/task")
						git(t, twin, "commit", "-q", "-m", "task: task work")
					} else {
						git(t, twin, "merge", "--no-ff", "--no-edit", "hrn/task")
					}
					res := Result{Dir: dir, Tasks: []TaskResult{{ID: "task", Prompt: "task work", Branch: "hrn/task", Commit: head, Status: StatusPass}}}
					if out, err := Merge(context.Background(), res, nil, squash); err != nil || len(out) != 1 || out[0].Commit == "" {
						t.Fatalf("Merge = %+v, %v", out, err)
					}
					if got, want := git(t, dir, "log", "-1", "--format=%B"), git(t, twin, "log", "-1", "--format=%B"); got != want {
						t.Errorf("message = %q, want %q", got, want)
					}
					if got, want := git(t, dir, "reflog", "-1", "--format=%gs"), git(t, twin, "reflog", "-1", "--format=%gs"); got != want {
						t.Errorf("reflog = %q, want %q", got, want)
					}
				})
			}
		}
	}
}

func TestMergeKeepsCallerAuthorshipLines(t *testing.T) {
	dir := initRepo(t)
	res := mergeable(t, dir, Task{ID: "alpha", Prompt: "alpha"})
	git(t, dir, "config", "merge.log", "true")
	const message = "Reviewed task\n\n# By the caller\n# Via the caller"
	if _, err := merge(context.Background(), res, nil, false, message); err != nil {
		t.Fatal(err)
	}
	if got := git(t, dir, "log", "-1", "--format=%B"); !strings.HasPrefix(got, message+"\n") {
		t.Errorf("caller message changed: %q", got)
	}
}

func TestMergeConflictsDoNotIncludeTheMessageOrShortlog(t *testing.T) {
	for _, message := range []string{"", "private merge explanation"} {
		t.Run(message, func(t *testing.T) {
			dir := initRepo(t)
			git(t, dir, "switch", "-q", "-c", "hrn/task")
			writeFile(t, filepath.Join(dir, "README.md"), "task\n")
			git(t, dir, "commit", "-q", "-a", "-m", "private shortlog entry")
			head := git(t, dir, "rev-parse", "HEAD")
			git(t, dir, "switch", "-q", "main")
			writeFile(t, filepath.Join(dir, "README.md"), "main\n")
			git(t, dir, "commit", "-q", "-a", "-m", "main work")
			git(t, dir, "config", "merge.log", "true")
			res := Result{Dir: dir, Tasks: []TaskResult{{ID: "task", Branch: "hrn/task", Commit: head, Status: StatusPass}}}
			out, err := merge(context.Background(), res, nil, false, message)
			if err == nil || len(out) != 1 || !out[0].Conflict {
				t.Fatalf("merge = %+v, %v", out, err)
			}
			for _, text := range []string{err.Error(), out[0].Error} {
				for _, unwanted := range []string{"private merge explanation", "private shortlog entry", "Merge branch 'hrn/task'", "* hrn/task:"} {
					if strings.Contains(text, unwanted) {
						t.Errorf("error includes %q: %s", unwanted, text)
					}
				}
				if !strings.Contains(text, "CONFLICT (content)") {
					t.Errorf("error lost git's conflict: %s", text)
				}
			}
		})
	}
}

func TestMergeSkipsNamedTasksWithoutRecordedBranches(t *testing.T) {
	for _, status := range []Status{StatusFail, StatusPass} {
		t.Run(string(status), func(t *testing.T) {
			dir := initRepo(t)
			before := git(t, dir, "rev-parse", "HEAD")
			res := Result{Dir: dir, Tasks: []TaskResult{{ID: "task", Status: status}}}
			out, err := Merge(context.Background(), res, []string{"task"}, false)
			want := "changed nothing"
			if status != StatusPass {
				want = "did not pass (" + string(status) + ")"
			}
			if err != nil || len(out) != 1 || out[0].Skipped != want || out[0].Error != "" {
				t.Fatalf("Merge = %+v, %v; want %q", out, err, want)
			}
			if got := git(t, dir, "rev-parse", "HEAD"); got != before {
				t.Errorf("HEAD moved to %s", got)
			}
		})
	}
}

func TestCommitChangesLeavesADashNamedUnbornBranchAlone(t *testing.T) {
	dir := initRepo(t)
	git(t, dir, "branch", "task")
	git(t, dir, "switch", "--orphan", "unborn")
	git(t, dir, "symbolic-ref", "HEAD", "refs/heads/-unborn")
	if sha, note, err := commitChanges(dir, "task", "task work"); err != nil || sha != "" || note != "" {
		t.Errorf("commitChanges = %q, %q, %v; want no change", sha, note, err)
	}
	if got := git(t, dir, "symbolic-ref", "HEAD"); got != "refs/heads/-unborn" {
		t.Errorf("unborn branch changed: %s", got)
	}
}

func TestMergeRejectsInvalidPassingBranchesBeforeMergingAnyTask(t *testing.T) {
	dir := initRepo(t)
	res := mergeable(t, dir, Task{ID: "alpha", Prompt: "alpha"})
	before := git(t, dir, "rev-parse", "HEAD")
	res.Tasks = append(res.Tasks, TaskResult{ID: "invalid", Status: StatusPass, Commit: res.Tasks[0].Commit})
	if out, err := Merge(context.Background(), res, nil, false); err == nil || !strings.Contains(err.Error(), "invalid recorded branch") || len(out) != 0 {
		t.Errorf("Merge = %+v, %v; want refusal before any merge", out, err)
	}
	if got := git(t, dir, "rev-parse", "HEAD"); got != before {
		t.Errorf("HEAD moved to %s", got)
	}
}

func TestDroppingABranchDoesNotExpandCheckoutHistory(t *testing.T) {
	dir := initRepo(t)
	git(t, dir, "switch", "-c", "previous")
	git(t, dir, "switch", "main")
	dropBranch(dir, "@{-1}")
	git(t, dir, "show-ref", "--verify", "refs/heads/previous")
}

func TestTaskBranchCreationDoesNotExpandCheckoutHistory(t *testing.T) {
	for _, mode := range []string{"fresh", "slot"} {
		t.Run(mode, func(t *testing.T) {
			dir := initRepo(t)
			base := git(t, dir, "rev-parse", "HEAD")
			git(t, dir, "switch", "-c", "previous")
			git(t, dir, "switch", "main")
			git(t, dir, "branch", "-D", "previous")
			var err error
			if mode == "fresh" {
				err = addWorktree(context.Background(), dir, TaskResult{Branch: "@{-1}", Worktree: filepath.Join(t.TempDir(), "wt")}, base)
			} else {
				_, err = takeSlot(context.Background(), &Spec{Parallel: 1}, "batch", &keptSlots{}, "task", "@{-1}", dir, base, func(Event) {}, func(bool) error { return nil })
			}
			if err == nil {
				t.Fatal("created a branch from checkout history")
			}
			if refs := git(t, dir, "for-each-ref", "--format=%(refname)", "refs/heads/previous"); refs != "" {
				t.Errorf("created previous branch: %s", refs)
			}
		})
	}
}

func TestRemovalClaimsRequireAnExactBranchDespiteTagsAndDescendants(t *testing.T) {
	dir := initRepo(t)
	git(t, dir, "tag", "refs/heads/parent", "HEAD")
	git(t, dir, "branch", "parent/child")
	res := Result{Dir: dir, Tasks: []TaskResult{{ID: "task", Branch: "parent", Slot: 1}}}
	removed, err := RemoveTasks(res, []string{"task"})
	if err != nil || len(removed) != 0 {
		t.Errorf("RemoveTasks = %v, %v", removed, err)
	}
	git(t, dir, "show-ref", "--verify", "refs/heads/parent/child")
	git(t, dir, "show-ref", "--verify", "refs/tags/refs/heads/parent")
}

func TestMergeReceiptsReadParentsDespiteCollidingFiles(t *testing.T) {
	dir := initRepo(t)
	res := mergeable(t, dir, Task{ID: "alpha", Prompt: "alpha"})
	out, err := Merge(context.Background(), res, nil, false)
	if err != nil || len(out) != 1 || out[0].Commit == "" {
		t.Fatalf("Merge = %v, %v", out, err)
	}
	for _, parent := range []string{"^1", "^2"} {
		if err := os.WriteFile(filepath.Join(dir, out[0].Commit+parent), []byte("collision\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commit, unchanged, err := readMergeReceipt(dir, out[0].Before, res.Tasks[0].Commit, false)
	if err != nil || unchanged || commit != out[0].Commit {
		t.Errorf("receipt = %s, %v, %v", commit, unchanged, err)
	}
}

func TestSlotBranchCreationReadsItsBaseOnlyAsARevision(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := initRepo(t)
	base := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "update-ref", "refs/heads/-base", base)
	got, err := takeSlot(context.Background(), &Spec{Parallel: 1}, "batch", &keptSlots{}, "task", "task", dir, "-base", func(Event) {}, func(bool) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if head := git(t, got.Path, "rev-parse", "HEAD"); head != base {
		t.Errorf("slot HEAD = %s, want %s", head, base)
	}
	if err := giveBack(got, base); err != nil {
		t.Fatal(err)
	}
	dropBranch(dir, "task")
}

func TestSetupCommandsKeepResolvedCommitsSeparateFromFiles(t *testing.T) {
	dir, base, head := setupRepo(t)
	for _, revision := range []string{base, head, head + "^{tree}"} {
		if err := os.WriteFile(filepath.Join(dir, revision), []byte("collision\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := SetupCommit(context.Background(), dir, head, base, DefaultSetupPaths)
	if err != nil || got.Commit == "" || got.Source != base || len(got.Changed) == 0 {
		t.Fatalf("SetupCommit = %+v, %v", got, err)
	}
	if parent := git(t, dir, "rev-parse", got.Commit+"^1"); parent != head {
		t.Errorf("setup parent = %s, want %s", parent, head)
	}
}

func TestSetupReadsOnlyTheRequestedPathWhenADoubleDashFileExists(t *testing.T) {
	dir := initRepo(t)
	writeFile(t, filepath.Join(dir, "--"), "keep\n")
	writeFile(t, filepath.Join(dir, "AGENTS.md"), "old rules\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "base")
	base := git(t, dir, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(dir, "AGENTS.md"), "new rules\n")
	git(t, dir, "commit", "-q", "-a", "-m", "head")
	head := git(t, dir, "rev-parse", "HEAD")
	got, err := SetupCommit(context.Background(), dir, head, base, []string{"AGENTS.md"})
	if err != nil {
		t.Fatal(err)
	}
	if body := git(t, dir, "show", got.Commit+":AGENTS.md"); body != "old rules" {
		t.Errorf("setup instructions = %q", body)
	}
	if body := git(t, dir, "show", got.Commit+":--"); body != "keep" {
		t.Errorf("unselected file = %q", body)
	}
}

func forbidRevisionGit(t *testing.T) func() {
	t.Helper()
	trace := filepath.Join(t.TempDir(), "git-trace.json")
	t.Setenv("GIT_TRACE2_EVENT", filepath.ToSlash(trace))
	return func() {
		t.Helper()
		data, err := os.ReadFile(trace)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if len(data) != 0 {
			t.Errorf("invalid recorded branch started git:\n%s", data)
		}
	}
}

func TestRecordedBranchesAreRejectedBeforeGitStarts(t *testing.T) {
	for _, branch := range []string{"", "--force"} {
		for _, operation := range []string{"merge", "merge with message", "merge at", "branch head", "remove", "remove slot", "remove worktree", "add worktree", "drop branch"} {
			t.Run(operation+"/"+branch, func(t *testing.T) {
				dir := initRepo(t)
				head := git(t, dir, "rev-parse", "HEAD")
				tr := TaskResult{ID: "task", Branch: branch, Commit: head, Status: StatusPass, Slot: 1, BranchMade: true, Worktree: filepath.Join(t.TempDir(), "worktree")}
				res := Result{Dir: dir, Tasks: []TaskResult{tr}}
				check := forbidRevisionGit(t)
				var err error
				switch operation {
				case "merge":
					_, err = Merge(context.Background(), res, nil, false)
				case "merge with message":
					_, err = merge(context.Background(), res, nil, false, "chosen message")
				case "merge at":
					_, err = MergeAt(context.Background(), res, tr.ID, head)
				case "branch head":
					_, err = BranchHead(context.Background(), res, tr.ID)
				case "remove":
					err = Remove(res)
				case "remove slot":
					err = removeSlotBranch(dir, tr)
				case "remove worktree":
					err = removeWorktree(dir, tr)
				case "add worktree":
					err = addWorktree(context.Background(), dir, tr, head)
				case "drop branch":
					dropBranch(dir, branch)
				}
				check()
				if operation != "drop branch" && (err == nil || !strings.Contains(err.Error(), "invalid recorded branch") || !strings.Contains(err.Error(), fmt.Sprintf("%q", branch))) {
					t.Errorf("%s = %v; want an error naming invalid recorded branch %q", operation, err, branch)
				}
			})
		}
	}
}

func TestMergeWithAMessageUsesTheRecordedBranchAndConfirmsItsTip(t *testing.T) {
	dir := initRepo(t)
	res := mergeable(t, dir, Task{ID: "alpha", Prompt: "alpha"})
	tr := res.Tasks[0]
	git(t, dir, "tag", tr.Branch, res.Base)
	git(t, dir, "config", "merge.log", "true")
	const message = "Reviewed task\n\nKeep this explanation."
	merged, err := merge(context.Background(), res, nil, false, message)
	if err != nil || len(merged) != 1 || merged[0].Commit == "" || merged[0].Skipped != "" {
		t.Fatalf("merge = %+v, %v", merged, err)
	}
	if got := git(t, dir, "rev-parse", "HEAD^2"); got != tr.Commit {
		t.Errorf("second parent = %s, want %s", got, tr.Commit)
	}
	if got := git(t, dir, "log", "-1", "--format=%B"); !strings.HasPrefix(got, message) {
		t.Errorf("message = %q, want prefix %q", got, message)
	}
	subject := git(t, dir, "log", "-1", "--format=%s", tr.Commit, "--")
	if got := git(t, dir, "log", "-1", "--format=%B"); strings.Count(got, "\n  "+subject) != 1 {
		t.Errorf("configured shortlog must occur once: %q", got)
	}
	if got := git(t, dir, "rev-parse", "HEAD"); got != merged[0].Commit {
		t.Errorf("receipt = %s, want %s", merged[0].Commit, got)
	}
}

func TestMergeAtKeepsTheConfirmedCommitAndBranchMessageWithCollidingRefs(t *testing.T) {
	for _, shortlog := range []bool{false, true} {
		t.Run(fmt.Sprint(shortlog), func(t *testing.T) {
			dir := initRepo(t)
			res := mergeable(t, dir, Task{ID: "alpha", Prompt: "alpha"})
			tr := res.Tasks[0]
			git(t, dir, "tag", tr.Branch, res.Base)
			git(t, dir, "config", "merge.log", fmt.Sprint(shortlog))
			subject := git(t, dir, "log", "-1", "--format=%s", tr.Commit, "--")
			previous := mergeStep
			moved := false
			mergeStep = func(ctx context.Context, g *agentGit, args ...string) (string, error) {
				if len(args) > 0 && args[0] == "merge" && !moved {
					git(t, dir, "update-ref", "refs/heads/"+tr.Branch, res.Base, tr.Commit)
					moved = true
				}
				return previous(ctx, g, args...)
			}
			t.Cleanup(func() { mergeStep = previous })
			if err := os.WriteFile(filepath.Join(dir, tr.Commit), []byte("revision collision\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			merged, err := MergeAt(context.Background(), res, tr.ID, tr.Commit)
			if err != nil || len(merged) != 1 || merged[0].Commit == "" || merged[0].Skipped != "" || merged[0].Branch != tr.Branch {
				t.Fatalf("MergeAt = %+v, %v", merged, err)
			}
			if got := git(t, dir, "rev-parse", "HEAD^2"); got != tr.Commit {
				t.Errorf("second parent = %s, want %s", got, tr.Commit)
			}
			if got, want := git(t, dir, "log", "-1", "--format=%s"), "Merge branch '"+tr.Branch+"'"; got != want {
				t.Errorf("subject = %q, want %q", got, want)
			}
			body := git(t, dir, "log", "-1", "--format=%B")
			wantEntries := 0
			if shortlog {
				wantEntries = 1
			}
			if got := strings.Count(body, "\n  "+subject); got != wantEntries {
				t.Errorf("shortlog entries = %d, want %d in %q", got, wantEntries, body)
			}
			if !moved {
				t.Error("merge did not exercise movement after confirmation")
			}
			if got := git(t, dir, "rev-parse", "HEAD"); got != merged[0].Commit {
				t.Errorf("receipt = %s, want %s", merged[0].Commit, got)
			}
			if res.Tasks[0].Branch != tr.Branch {
				t.Error("MergeAt changed the caller's recorded branch")
			}
		})
	}
}

func TestBranchHeadRequiresTheExactRecordedBranch(t *testing.T) {
	for _, branch := range []string{"topic", "topic~1", "missing", "parent"} {
		t.Run(branch, func(t *testing.T) {
			dir := initRepo(t)
			base := git(t, dir, "rev-parse", "HEAD")
			head := git(t, dir, "commit-tree", base+"^{tree}", "-p", base, "-m", "topic")
			git(t, dir, "branch", "topic", head)
			git(t, dir, "tag", "topic", base)
			git(t, dir, "tag", "refs/heads/missing", head)
			git(t, dir, "branch", "parent/child", head)
			res := Result{Dir: dir, Tasks: []TaskResult{{ID: "task", Branch: branch}}}
			got, err := BranchHead(context.Background(), res, "task")
			if branch == "topic" {
				if err != nil || got != head {
					t.Errorf("BranchHead = %q, %v; want %s", got, err, head)
				}
			} else if err == nil {
				t.Errorf("BranchHead accepted %q as a branch: %s", branch, got)
			}
		})
	}
}

func TestRemovalUsesOnlyTheRecordedBranchWhenTagsAndFilesCollide(t *testing.T) {
	for _, operation := range []string{"slot", "worktree", "drop"} {
		t.Run(operation, func(t *testing.T) {
			dir := initRepo(t)
			git(t, dir, "branch", "topic")
			git(t, dir, "tag", "topic")
			if err := os.WriteFile(filepath.Join(dir, "topic"), []byte("keep\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			tr := TaskResult{Branch: "topic", Worktree: t.TempDir()}
			var err error
			switch operation {
			case "slot":
				err = removeSlotBranch(dir, tr)
			case "worktree":
				err = removeWorktree(dir, tr)
			case "drop":
				dropBranch(dir, tr.Branch)
			}
			if err != nil {
				t.Fatal(err)
			}
			if out := git(t, dir, "for-each-ref", "--format=%(refname)", "refs/heads/topic"); out != "" {
				t.Errorf("branch remains: %s", out)
			}
			git(t, dir, "show-ref", "--verify", "refs/tags/topic")
			if data, err := os.ReadFile(filepath.Join(dir, "topic")); err != nil || string(data) != "keep\n" {
				t.Errorf("colliding file = %q, %v", data, err)
			}
		})
	}
}

func TestBranchOwnershipDoesNotTreatATagOrRevisionAsARecordedBranch(t *testing.T) {
	for _, branch := range []string{"missing", "topic~1"} {
		t.Run(branch, func(t *testing.T) {
			dir := initRepo(t)
			git(t, dir, "branch", "topic")
			git(t, dir, "commit", "-q", "--allow-empty", "-m", "next")
			git(t, dir, "branch", "-f", "topic", "HEAD")
			git(t, dir, "tag", "refs/heads/missing", "HEAD")
			tr := TaskResult{Branch: branch, Slot: 1, Worktree: filepath.Join(t.TempDir(), "absent")}
			if made, left := stillMade(dir, tr); made {
				t.Errorf("stillMade = true, %q without an exact branch", left)
			}
			if WorktreesRemain(Result{Dir: dir, Tasks: []TaskResult{tr}}) {
				t.Error("WorktreesRemain found a tag or revision instead of a branch")
			}
			for _, remove := range []func(string, TaskResult) error{removeSlotBranch, removeWorktree} {
				if err := remove(dir, tr); err != nil {
					t.Errorf("removing an absent branch: %v", err)
				}
			}
			git(t, dir, "show-ref", "--verify", "refs/heads/topic")
			git(t, dir, "show-ref", "--verify", "refs/tags/refs/heads/missing")
		})
	}
}

func TestWorktreeCreationReadsItsBaseOnlyAsARevision(t *testing.T) {
	dir := initRepo(t)
	base := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "update-ref", "refs/heads/-base", base)
	git(t, dir, "tag", "topic", base)
	tr := TaskResult{Branch: "topic", Worktree: filepath.Join(t.TempDir(), "worktree")}
	if err := addWorktree(context.Background(), dir, tr, "-base"); err != nil {
		t.Fatal(err)
	}
	if got := git(t, tr.Worktree, "symbolic-ref", "HEAD"); got != "refs/heads/topic" {
		t.Errorf("worktree branch = %q", got)
	}
	if got := git(t, tr.Worktree, "rev-parse", "HEAD"); got != base {
		t.Errorf("worktree HEAD = %s, want %s", got, base)
	}
}

func TestFailedWorktreeCreationDeletesOnlyTheBranchItCreated(t *testing.T) {
	dir := initRepo(t)
	base := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "tag", "topic", base)
	occupied := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(occupied, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := TaskResult{Branch: "topic", Worktree: occupied}
	if err := addWorktree(context.Background(), dir, tr, base); err == nil {
		t.Fatal("addWorktree accepted a file as a worktree")
	}
	if got := git(t, dir, "for-each-ref", "--format=%(refname)", "refs/heads/topic"); got != "" {
		t.Errorf("created branch survived cleanup: %s", got)
	}
	git(t, dir, "show-ref", "--verify", "refs/tags/topic")
	if data, err := os.ReadFile(occupied); err != nil || string(data) != "keep\n" {
		t.Errorf("occupied path = %q, %v", data, err)
	}
}

func TestProtectedFilesReadTheBaseOnlyAsARevision(t *testing.T) {
	dir := initRepo(t)
	written := filepath.Join(t.TempDir(), "written")
	_, err := changedProtected(context.Background(), dir, protection{base: "--output=" + written, globs: []string{"*"}})
	if err == nil {
		t.Error("protected-file diff accepted an option as its base")
	}
	if _, err := os.Stat(written); !os.IsNotExist(err) {
		t.Errorf("git wrote %s: %v", written, err)
	}
}

func TestAgentBranchReadsIgnoreTagsAndDescendantBranches(t *testing.T) {
	dir := initRepo(t)
	base := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "switch", "-q", "-c", "task")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "task")
	head := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "tag", "task", base)
	git(t, dir, "branch", "parent/child", head)
	git(t, dir, "tag", "refs/heads/parent", head)
	if tips, err := gitTips(dir, "task"); err != nil || tips != head+" "+head {
		t.Errorf("gitTips = %q, %v; want the task branch and HEAD", tips, err)
	}
	if tips, err := gitTips(dir, "parent"); err != nil || tips != " "+head {
		t.Errorf("gitTips found a nonexistent parent branch: %q, %v", tips, err)
	}
	if sha, note, err := agentCommit(dir, "parent", base); err != nil || sha != "" || note != "" {
		t.Errorf("agentCommit found a nonexistent parent branch: %q, %q, %v", sha, note, err)
	}
	if sha, note, err := commitChanges(dir, "parent", "task"); err != nil || sha != "" || !strings.Contains(note, "left in the worktree") {
		t.Errorf("commitChanges lost the off-branch commit: %q, %q, %v", sha, note, err)
	}
}

func TestDiffReadsRecordedRangesAndAnOrphanBaseOnlyAsRevisions(t *testing.T) {
	for _, orphan := range []bool{false, true} {
		t.Run(fmt.Sprint(orphan), func(t *testing.T) {
			dir := initRepo(t)
			base := git(t, dir, "rev-parse", "HEAD")
			git(t, dir, "update-ref", "refs/heads/-base", base)
			git(t, dir, "switch", "-q", "-c", "task")
			if err := os.WriteFile(filepath.Join(dir, "change.txt"), []byte("task change\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			git(t, dir, "add", "change.txt")
			git(t, dir, "commit", "-q", "-m", "task")
			head := git(t, dir, "rev-parse", "HEAD")
			tr := TaskResult{ID: "task", Commit: head, Worktree: dir}
			res := Result{Dir: dir, Base: "-base", Tasks: []TaskResult{tr}}
			if orphan {
				git(t, dir, "checkout", "-q", "--orphan", "unborn")
				res.Tasks[0].Commit = ""
			} else if err := os.WriteFile(filepath.Join(dir, "-base.."+head), []byte("range collision\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			diff, _, err := Diff(context.Background(), res, "task")
			if err != nil || !strings.Contains(diff, "+task change") {
				t.Errorf("Diff = %q, %v; want the task change", diff, err)
			}
		})
	}
}

func TestBatchStartupDoesNotMistakeATagForAnExistingTaskBranch(t *testing.T) {
	for _, worktrees := range []string{WorktreesSlots, WorktreesFresh} {
		t.Run(worktrees, func(t *testing.T) {
			dir := initRepo(t)
			git(t, dir, "tag", "refs/heads/hrn/task", "HEAD")
			res, _ := run(t, Spec{Dir: dir, Backend: "wb-bad", Retry: []string{"wb-file"}, Worktrees: worktrees, Tasks: []Task{{ID: "task", Prompt: "task"}}})
			if len(res.Tasks) != 1 || res.Tasks[0].Status != StatusPass || res.Tasks[0].Commit == "" || len(res.Tasks[0].Attempts) != 2 {
				t.Fatalf("batch did not create the available branch: %+v", res.Tasks)
			}
			if got := git(t, dir, "show-ref", "--hash", "--verify", "refs/heads/hrn/task"); got != res.Tasks[0].Commit {
				t.Errorf("created branch = %s, want %s", got, res.Tasks[0].Commit)
			}
			git(t, dir, "show-ref", "--verify", "refs/tags/refs/heads/hrn/task")
		})
	}
}
