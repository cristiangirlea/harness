package batch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
)

// offBranch registers a backend that moves HEAD off the task's branch with
// move, leaves out.txt uncommitted and ends with status.
func offBranch(name string, move []string, status agent.Status) string {
	agent.Register(stub{name, func(_ context.Context, task agent.Task) agent.Result {
		if len(move) > 0 {
			if err := gitIn(task.Dir, move...); err != nil {
				return agent.Result{Backend: name, Status: agent.StatusError, Error: err.Error()}
			}
		}
		_ = os.WriteFile(filepath.Join(task.Dir, "out.txt"), []byte("x\n"), 0o644)
		return agent.Result{Backend: name, Status: status, Final: "wrote off the branch", Error: "maybe", Turns: 1}
	}})
	return name
}

var offMoves = map[string][]string{
	"detached":     {"checkout", "-q", "--detach"},
	"other branch": {"checkout", "-q", "-b", "agent-side"},
	"orphan":       {"checkout", "-q", "--orphan", "agent-orphan"},
}

// assertLeftOff checks that a task whose agent left work off its branch ended
// as error saying so, recorded and merged nothing, and kept the work.
func assertLeftOff(t *testing.T, dir string, res Result, attempts int) {
	t.Helper()
	tr := res.Tasks[0]
	if tr.Status != StatusError || !strings.Contains(tr.Error, "not the task's branch") || tr.Commit != "" {
		t.Errorf("status %s, commit %q, error %q: want error saying the work is off the branch", tr.Status, tr.Commit, tr.Error)
	}
	if len(tr.Attempts) != attempts {
		t.Errorf("attempts = %d, want %d: no retry may remove or reuse the worktree", len(tr.Attempts), attempts)
	}
	if _, err := os.Stat(filepath.Join(tr.Worktree, "out.txt")); err != nil {
		t.Errorf("the agent's file is gone: %v", err)
	}
	if tip := git(t, dir, "rev-parse", "refs/heads/"+tr.Branch); tip != res.Base {
		t.Errorf("the task's branch moved to %s", tip)
	}
	merged, err := Merge(context.Background(), res, nil, false)
	if err == nil || !strings.Contains(err.Error(), "no task that passed") || len(merged) != 0 {
		t.Errorf("merged = %+v, %v; want nothing to merge", merged, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); !os.IsNotExist(err) {
		t.Errorf("out.txt reached the repository: %v", err)
	}
}

func TestWorkLeftOffTheTasksBranchEndsTheTaskAsAnError(t *testing.T) {
	for name, move := range offMoves {
		for _, mode := range []string{WorktreesFresh, WorktreesSlots} {
			t.Run(name+" "+mode, func(t *testing.T) {
				dir := initRepo(t)
				backend := offBranch("wb-off-"+strings.ReplaceAll(name, " ", "-"), move, agent.StatusOK)
				res, _ := run(t, Spec{Dir: dir, Backend: backend, Worktrees: mode, Tasks: []Task{{ID: "off", Prompt: "p"}}})
				defer Remove(res)
				assertLeftOff(t, dir, res, 1)
				if mode == WorktreesSlots && !strings.Contains(res.Tasks[0].Error, "kept") {
					t.Errorf("error %q: a slot holding the work must be kept", res.Tasks[0].Error)
				}
			})
		}
	}
}

func TestWorkLeftOffTheBranchIsNotRetriedAway(t *testing.T) {
	for _, mode := range []string{WorktreesFresh, WorktreesSlots} {
		t.Run(mode, func(t *testing.T) {
			dir := initRepo(t)
			backend := offBranch("wb-off-errs", offMoves["detached"], agent.StatusError)
			res, _ := run(t, Spec{Dir: dir, Backend: backend, Retry: []string{"wb-ok"}, Worktrees: mode, Tasks: []Task{{ID: "off", Prompt: "p"}}})
			defer Remove(res)
			assertLeftOff(t, dir, res, 1)
			if mode == WorktreesSlots && !strings.Contains(res.Tasks[0].Error, "kept") {
				t.Errorf("error %q: a slot holding the work must be kept", res.Tasks[0].Error)
			}
		})
	}
}

func TestWorkLeftOffTheBranchWithAFailingGateIsNotContinued(t *testing.T) {
	dir := initRepo(t)
	backend := offBranch("wb-off-gate", offMoves["detached"], agent.StatusOK)
	res, _ := run(t, Spec{Dir: dir, Backend: backend, Retry: []string{"wb-ok"}, Worktrees: WorktreesFresh,
		Gate: []string{"exit 3"}, Tasks: []Task{{ID: "off", Prompt: "p"}}})
	defer Remove(res)
	assertLeftOff(t, dir, res, 1)
}

func TestACancelledTaskWithWorkOffItsBranchKeepsTheWork(t *testing.T) {
	for _, mode := range []string{WorktreesFresh, WorktreesSlots} {
		t.Run(mode, func(t *testing.T) {
			dir := initRepo(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			name := "wb-off-cancel-" + mode
			agent.Register(stub{name, func(_ context.Context, task agent.Task) agent.Result {
				_ = gitIn(task.Dir, "checkout", "-q", "--detach")
				_ = os.WriteFile(filepath.Join(task.Dir, "out.txt"), []byte("x\n"), 0o644)
				cancel()
				return agent.Result{Backend: name, Status: agent.StatusCanceled, Error: "context canceled"}
			}})
			res, _ := Run(ctx, Spec{Dir: dir, Backend: name, Worktrees: mode, Tasks: []Task{{ID: "off", Prompt: "p"}}}, nil)
			defer Remove(res)
			tr := res.Tasks[0]
			if tr.Status != StatusCanceled || tr.Commit != "" {
				t.Errorf("status %s, commit %q: want canceled with nothing recorded", tr.Status, tr.Commit)
			}
			if _, err := os.Stat(filepath.Join(tr.Worktree, "out.txt")); err != nil {
				t.Errorf("the agent's file is gone: %v", err)
			}
			if tip := git(t, dir, "rev-parse", "refs/heads/"+tr.Branch); tip != res.Base {
				t.Errorf("the task's branch moved to %s", tip)
			}
		})
	}
}

func TestMovingOffTheBranchWithNothingWrittenChangesNothing(t *testing.T) {
	for name, move := range map[string][]string{"detached": offMoves["detached"], "other branch": offMoves["other branch"]} {
		for _, mode := range []string{WorktreesFresh, WorktreesSlots} {
			t.Run(name+" "+mode, func(t *testing.T) {
				dir := initRepo(t)
				backend := "wb-moves-" + strings.ReplaceAll(name, " ", "-")
				agent.Register(stub{backend, func(_ context.Context, task agent.Task) agent.Result {
					if err := gitIn(task.Dir, move...); err != nil {
						return agent.Result{Backend: backend, Status: agent.StatusError, Error: err.Error()}
					}
					return agent.Result{Backend: backend, Status: agent.StatusOK, Final: "moved", Turns: 1}
				}})
				res, _ := run(t, Spec{Dir: dir, Backend: backend, Worktrees: mode, Tasks: []Task{{ID: "moved", Prompt: "p"}}})
				defer Remove(res)
				if tr := res.Tasks[0]; tr.Status != StatusPass || tr.Commit != "" || strings.Contains(tr.Error, "kept") {
					t.Errorf("status %s, commit %q, error %q: want a pass with nothing recorded and the slot given back", tr.Status, tr.Commit, tr.Error)
				}
			})
		}
	}
}
