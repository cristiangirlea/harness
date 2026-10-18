package batch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/limits"
)

func init() {
	// wb-commits commits its own change, so hrn finds a clean tree afterwards.
	agent.Register(stub{"wb-commits", func(_ context.Context, task agent.Task) agent.Result {
		_ = os.WriteFile(filepath.Join(task.Dir, "out.txt"), []byte(typed(task)+"\n"), 0o644)
		for _, args := range [][]string{{"add", "out.txt"}, {"commit", "-q", "-m", "agent's own commit"}} {
			if out, err := exec.Command("git", append([]string{"-C", task.Dir}, args...)...).CombinedOutput(); err != nil {
				return agent.Result{Backend: "wb-commits", Status: agent.StatusError, Error: string(out)}
			}
		}
		return agent.Result{Backend: "wb-commits", Status: agent.StatusOK, Final: "committed out.txt", Turns: 1}
	}})
}

func TestATaskWhoseAgentCommittedItsOwnWorkRecordsThatCommit(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-commits", Worktrees: WorktreesFresh, Gate: []string{"test -f out.txt"},
		Tasks: []Task{{ID: "own", Prompt: "write out"}}})
	defer Remove(res)
	tr := res.Tasks[0]
	if tr.Status != StatusPass || tr.Commit == "" {
		t.Fatalf("status %s, commit %q: want a pass that records the agent's commit", tr.Status, tr.Commit)
	}
	if head := git(t, tr.Worktree, "rev-parse", "HEAD"); tr.Commit != head {
		t.Errorf("commit = %s, want the worktree's HEAD %s", tr.Commit, head)
	}
	diff, _, err := Diff(context.Background(), res, "own")
	if err != nil || !strings.Contains(diff, "+++ b/out.txt") {
		t.Fatalf("diff = %q, %v; want the agent's change", diff, err)
	}
	merged, err := Merge(context.Background(), res, nil, false)
	if err != nil || len(merged) != 1 || merged[0].Commit == "" || merged[0].Skipped != "" {
		t.Fatalf("merged = %+v, %v; want the agent's commit merged", merged, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); err != nil {
		t.Errorf("out.txt did not arrive in the repository: %v", err)
	}
}

func TestATaskThatChangedNothingStillRecordsNoCommit(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-noop", Worktrees: WorktreesFresh, Tasks: []Task{{ID: "idle", Prompt: "p"}}})
	defer Remove(res)
	if c := res.Tasks[0].Commit; c != "" {
		t.Errorf("commit = %s for a task that changed nothing", c)
	}
}

func TestACommitOnNoBranchIsNotRecordedAsTheTasks(t *testing.T) {
	dir := initRepo(t)
	agent.Register(stub{"wb-detached", func(_ context.Context, task agent.Task) agent.Result {
		_ = os.WriteFile(filepath.Join(task.Dir, "out.txt"), []byte("x\n"), 0o644)
		for _, args := range [][]string{{"checkout", "-q", "--detach"}, {"add", "out.txt"}, {"commit", "-q", "-m", "detached"}} {
			if out, err := exec.Command("git", append([]string{"-C", task.Dir}, args...)...).CombinedOutput(); err != nil {
				return agent.Result{Backend: "wb-detached", Status: agent.StatusError, Error: string(out)}
			}
		}
		return agent.Result{Backend: "wb-detached", Status: agent.StatusOK, Final: "committed detached", Turns: 1}
	}})
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-detached", Worktrees: WorktreesFresh, Tasks: []Task{{ID: "loose", Prompt: "p"}}})
	defer Remove(res)
	if c := res.Tasks[0].Commit; c != "" {
		t.Errorf("commit = %s, a commit on no branch that a merge would not bring", c)
	}
}

func gitIn(dir string, args ...string) error {
	_, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	return err
}

func init() {
	// wb-renames commits its work and then renames the branch it is on.
	agent.Register(stub{"wb-renames", func(_ context.Context, task agent.Task) agent.Result {
		_ = os.WriteFile(filepath.Join(task.Dir, "out.txt"), []byte("x\n"), 0o644)
		for _, args := range [][]string{{"add", "out.txt"}, {"commit", "-q", "-m", "own"}, {"branch", "-m", "renamed-by-agent"}} {
			if err := gitIn(task.Dir, args...); err != nil {
				return agent.Result{Backend: "wb-renames", Status: agent.StatusError, Error: err.Error()}
			}
		}
		return agent.Result{Backend: "wb-renames", Status: agent.StatusOK, Final: "renamed", Turns: 1}
	}})
	// wb-resets writes out.txt the first time and, continued, resets the branch to the root commit.
	agent.Register(stub{"wb-resets", func(_ context.Context, task agent.Task) agent.Result {
		if _, err := os.Stat(filepath.Join(task.Dir, "out.txt")); err == nil {
			root, _ := exec.Command("git", "-C", task.Dir, "rev-list", "--max-parents=0", "HEAD").Output()
			if err := gitIn(task.Dir, "reset", "-q", "--hard", strings.TrimSpace(string(root))); err != nil {
				return agent.Result{Backend: "wb-resets", Status: agent.StatusError, Error: err.Error()}
			}
			return agent.Result{Backend: "wb-resets", Status: agent.StatusOK, Final: "reset", Turns: 1}
		}
		_ = os.WriteFile(filepath.Join(task.Dir, "out.txt"), []byte("x\n"), 0o644)
		return agent.Result{Backend: "wb-resets", Status: agent.StatusOK, Final: "wrote", Turns: 1}
	}})
}

// failsOnce is a gate that fails its first run and passes after, wherever the worktree stands.
func failsOnce(t *testing.T) string {
	mark := filepath.ToSlash(filepath.Join(t.TempDir(), "once"))
	return `test -e "` + mark + `" || { touch "` + mark + `"; echo first; exit 3; }`
}

func TestAnAgentThatRenamesItsBranchKeepsItsGateResult(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-renames", Worktrees: WorktreesFresh, Tasks: []Task{{ID: "moved", Prompt: "p"}}})
	defer Remove(res)
	tr := res.Tasks[0]
	if tr.Status != StatusPass || tr.Commit != "" || tr.Error != "" {
		t.Errorf("status %s, commit %q, error %q: want a pass with no commit on the task's branch", tr.Status, tr.Commit, tr.Error)
	}
}

func TestAContinuedAttemptRecordsTheAgentsNewCommit(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-commits", Retry: []string{"wb-commits"}, Worktrees: WorktreesFresh,
		Gate: []string{failsOnce(t)}, Tasks: []Task{{ID: "again", Prompt: "write out"}}})
	defer Remove(res)
	tr := res.Tasks[0]
	if tr.Status != StatusPass || len(tr.Attempts) != 2 || !tr.Attempts[1].Continued {
		t.Fatalf("task = %s with %d attempts", tr.Status, len(tr.Attempts))
	}
	if tip := git(t, dir, "rev-parse", "refs/heads/"+tr.Branch); tr.Commit != tip {
		t.Errorf("commit = %s, want the branch tip %s after the agent's second commit", tr.Commit, tip)
	}
}

func TestAContinuedAttemptThatResetsToTheBaseRecordsNoCommit(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-resets", Retry: []string{"wb-resets"}, Worktrees: WorktreesFresh,
		Gate: []string{failsOnce(t)}, Tasks: []Task{{ID: "undone", Prompt: "p"}}})
	defer Remove(res)
	tr := res.Tasks[0]
	if tr.Status != StatusPass || len(tr.Attempts) != 2 {
		t.Fatalf("task = %s with %d attempts", tr.Status, len(tr.Attempts))
	}
	if tr.Commit != "" {
		t.Errorf("commit = %s, but the branch is back at the base", tr.Commit)
	}
	if diff, _, err := Diff(context.Background(), res, "undone"); err != nil || diff != "" {
		t.Errorf("diff = %q, %v; want none", diff, err)
	}
}

func TestASlotTaskWhoseAgentCommittedItsOwnWorkIsRecordedAndGivenBack(t *testing.T) {
	dir := initRepo(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-commits", Worktrees: WorktreesSlots, Gate: []string{"test -f out.txt"},
		Tasks: []Task{{ID: "slotted", Prompt: "write out"}}})
	defer Remove(res)
	tr := res.Tasks[0]
	if tr.Status != StatusPass || tr.Commit == "" || strings.Contains(tr.Error, "kept") {
		t.Fatalf("status %s, commit %q, error %q: want a recorded commit and the slot given back", tr.Status, tr.Commit, tr.Error)
	}
	diff, _, err := Diff(context.Background(), res, "slotted")
	if err != nil || !strings.Contains(diff, "+++ b/out.txt") {
		t.Fatalf("diff = %q, %v", diff, err)
	}
	if merged, err := Merge(context.Background(), res, nil, false); err != nil || len(merged) != 1 || merged[0].Commit == "" {
		t.Errorf("merged = %+v, %v", merged, err)
	}
}

// commitAll commits everything in dir with message, as an agent would.
func commitAll(dir, message string) error {
	if err := gitIn(dir, "add", "-A"); err != nil {
		return err
	}
	return gitIn(dir, "commit", "-q", "-m", message)
}

func TestACarriedOnAttemptRecordsThePartnersOwnCommit(t *testing.T) {
	dir := initRepo(t)
	for _, name := range append(limits.MeteredEnv("claude"), limits.MeteredEnv("codex")...) {
		t.Setenv(name, "")
	}
	for _, s := range []stub{
		{"codex", func(_ context.Context, task agent.Task) agent.Result {
			_ = os.WriteFile(filepath.Join(task.Dir, "half.txt"), []byte("first half\n"), 0o644)
			_ = commitAll(task.Dir, "first half")
			return agent.Result{Backend: "codex", Status: agent.StatusLimited, Error: "You've hit your usage limit.",
				LimitResets: time.Now().Add(time.Hour)}
		}},
		{"claude", func(_ context.Context, task agent.Task) agent.Result {
			_ = os.WriteFile(filepath.Join(task.Dir, "done.txt"), []byte("second half\n"), 0o644)
			if err := commitAll(task.Dir, "second half"); err != nil {
				return agent.Result{Backend: "claude", Status: agent.StatusError, Error: err.Error()}
			}
			return agent.Result{Backend: "claude", Status: agent.StatusOK, Final: "finished", Turns: 1}
		}},
	} {
		orig, err := agent.Get(s.name)
		if err != nil {
			t.Fatal(err)
		}
		agent.Register(s)
		t.Cleanup(func() { agent.Register(orig) })
	}
	res, _ := run(t, Spec{Dir: dir, Backend: "codex", Worktrees: WorktreesFresh,
		Gate: []string{"test -f half.txt && test -f done.txt"}, Tasks: []Task{{ID: "carried", Prompt: "both halves"}}})
	defer Remove(res)
	tr := res.Tasks[0]
	if tr.Status != StatusPass || len(tr.Attempts) != 2 {
		t.Fatalf("task = %s with %d attempts", tr.Status, len(tr.Attempts))
	}
	if tip := git(t, dir, "rev-parse", "refs/heads/"+tr.Branch); tr.Commit != tip {
		t.Errorf("commit = %s, want the branch tip %s holding both halves", tr.Commit, tip)
	}
}

func TestAFreshRetryAfterASelfCommittedFailureStartsWithNoCommit(t *testing.T) {
	dir := initRepo(t)
	agent.Register(stub{"wb-commits-fails", func(_ context.Context, task agent.Task) agent.Result {
		_ = os.WriteFile(filepath.Join(task.Dir, "out.txt"), []byte("x\n"), 0o644)
		_ = commitAll(task.Dir, "own")
		return agent.Result{Backend: "wb-commits-fails", Status: agent.StatusError, Error: "model exploded", Turns: 1}
	}})
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-commits-fails", Retry: []string{"wb-noop"}, Worktrees: WorktreesFresh,
		Tasks: []Task{{ID: "afresh", Prompt: "p"}}})
	defer Remove(res)
	tr := res.Tasks[0]
	if tr.Status != StatusPass || len(tr.Attempts) != 2 || tr.Attempts[1].Continued {
		t.Fatalf("task = %s, %d attempts", tr.Status, len(tr.Attempts))
	}
	if tr.Commit != "" {
		t.Errorf("commit = %s after a fresh retry that changed nothing", tr.Commit)
	}
	if diff, _, err := Diff(context.Background(), res, "afresh"); err != nil || diff != "" {
		t.Errorf("diff = %q, %v; want none", diff, err)
	}
}

func TestACancelledTaskKeepsTheCommitItsAgentMade(t *testing.T) {
	dir := initRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agent.Register(stub{"wb-commits-cancelled", func(_ context.Context, task agent.Task) agent.Result {
		_ = os.WriteFile(filepath.Join(task.Dir, "out.txt"), []byte("x\n"), 0o644)
		_ = commitAll(task.Dir, "own")
		cancel()
		return agent.Result{Backend: "wb-commits-cancelled", Status: agent.StatusCanceled, Error: "context canceled"}
	}})
	res, _ := Run(ctx, Spec{Dir: dir, Backend: "wb-commits-cancelled", Worktrees: WorktreesFresh, Tasks: []Task{{ID: "stopped", Prompt: "p"}}}, nil)
	defer Remove(res)
	tr := res.Tasks[0]
	if tr.Status != StatusCanceled || tr.Commit == "" {
		t.Errorf("status %s, commit %q: want canceled with the agent's commit recorded", tr.Status, tr.Commit)
	}
}

func TestARefBelowTheTasksBranchNameIsNotTakenForIt(t *testing.T) {
	dir := initRepo(t)
	agent.Register(stub{"wb-nests", func(_ context.Context, task agent.Task) agent.Result {
		_ = os.WriteFile(filepath.Join(task.Dir, "out.txt"), []byte("x\n"), 0o644)
		_ = commitAll(task.Dir, "own")
		branch, _ := exec.Command("git", "-C", task.Dir, "symbolic-ref", "--short", "HEAD").Output()
		b := strings.TrimSpace(string(branch))
		if err := gitIn(task.Dir, "branch", "-m", "elsewhere"); err != nil {
			return agent.Result{Backend: "wb-nests", Status: agent.StatusError, Error: err.Error()}
		}
		if err := gitIn(task.Dir, "branch", b+"/below"); err != nil {
			return agent.Result{Backend: "wb-nests", Status: agent.StatusError, Error: err.Error()}
		}
		return agent.Result{Backend: "wb-nests", Status: agent.StatusOK, Final: "nested", Turns: 1}
	}})
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-nests", Worktrees: WorktreesFresh, Tasks: []Task{{ID: "nest", Prompt: "p"}}})
	defer Remove(res)
	if tr := res.Tasks[0]; tr.Status != StatusPass || tr.Commit != "" {
		t.Errorf("status %s, commit %q: a ref below the branch name is not the task's branch", tr.Status, tr.Commit)
	}
}

func TestAWorktreeThatVanishedInAContinuedAttemptKeepsTheEarlierCommit(t *testing.T) {
	dir := initRepo(t)
	agent.Register(stub{"wb-vanishes", func(_ context.Context, task agent.Task) agent.Result {
		if _, err := os.Stat(filepath.Join(task.Dir, "out.txt")); err == nil {
			_ = os.RemoveAll(task.Dir)
			return agent.Result{Backend: "wb-vanishes", Status: agent.StatusOK, Final: "gone", Turns: 1}
		}
		_ = os.WriteFile(filepath.Join(task.Dir, "out.txt"), []byte("x\n"), 0o644)
		return agent.Result{Backend: "wb-vanishes", Status: agent.StatusOK, Final: "wrote", Turns: 1}
	}})
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-vanishes", Retry: []string{"wb-vanishes"}, Worktrees: WorktreesFresh,
		Gate: []string{failsOnce(t)}, Tasks: []Task{{ID: "vanished", Prompt: "p"}}})
	defer Remove(res)
	tr := res.Tasks[0]
	if len(tr.Attempts) != 2 || !tr.Attempts[1].Continued {
		t.Fatalf("task = %s with %d attempts", tr.Status, len(tr.Attempts))
	}
	if tip := git(t, dir, "rev-parse", "refs/heads/"+tr.Branch); tr.Commit != tip {
		t.Errorf("commit = %q, want the branch's commit %s from the first attempt", tr.Commit, tip)
	}
}
