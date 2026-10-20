package batch

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
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
	"detached":      {"checkout", "-q", "--detach"},
	"other branch":  {"checkout", "-q", "-b", "agent-side"},
	"orphan":        {"checkout", "-q", "--orphan", "agent-orphan"},
	"unicode space": {"checkout", "-q", "-b", "hrn/off\u00a0"},
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

func TestCommitChangesWithAFailingBranchReadEndsWithoutRetry(t *testing.T) {
	for _, mode := range []string{WorktreesFresh, WorktreesSlots} {
		t.Run(mode, func(t *testing.T) {
			dir := initRepo(t)
			name := "wb-branch-read-error-" + mode
			calls := 0
			agent.Register(stub{name, func(ctx context.Context, task agent.Task) agent.Result {
				calls++
				for _, args := range [][]string{
					{"update-ref", "refs/tags/off-head", "HEAD"},
					{"symbolic-ref", "HEAD", "refs/tags/off-head"},
				} {
					if err := gitIn(task.Dir, args...); err != nil {
						return agent.Result{Status: agent.StatusError, Error: err.Error()}
					}
				}
				if err := os.WriteFile(filepath.Join(task.Dir, "out.txt"), []byte("x\n"), 0o644); err != nil {
					return agent.Result{Status: agent.StatusError, Error: err.Error()}
				}
				g, err := newAgentGit(ctx, task.Dir)
				if err != nil {
					return agent.Result{Status: agent.StatusError, Error: err.Error()}
				}
				defer g.close()
				status, err := g.stdout(ctx, "status", "--porcelain", "-z", "--no-renames", "--ignore-submodules=all")
				if err != nil || status != "?? out.txt\x00" {
					return agent.Result{Status: agent.StatusError, Error: "status did not report out.txt"}
				}
				return agent.Result{Backend: name, Status: agent.StatusOK, Turns: 1}
			}})
			res, _ := run(t, Spec{Dir: dir, Backend: name, Retry: []string{name}, Worktrees: mode,
				Tasks: []Task{{ID: "off", Prompt: "p"}}})
			defer Remove(res)
			tr := res.Tasks[0]
			if tr.Status != StatusError || tr.Commit != "" || len(tr.Attempts) != 1 || calls != 1 {
				t.Fatalf("task = %+v, calls = %d; want one failed attempt without a commit", tr, calls)
			}
			for _, msg := range []string{tr.Error, tr.Attempts[0].Error} {
				if !strings.Contains(msg, "git branch --show-current:") || strings.Contains(msg, "detached") {
					t.Errorf("error = %q; want a git branch error, not a detached diagnosis", msg)
				}
			}
			if tr.Attempts[0].Result.Status != agent.StatusOK {
				t.Fatalf("stub failed before commitChanges: %+v", tr.Attempts[0].Result)
			}
			if tip := git(t, dir, "rev-parse", "refs/heads/"+tr.Branch); tip != res.Base {
				t.Errorf("task branch = %s, want base %s", tip, res.Base)
			}
			if head := git(t, tr.Worktree, "rev-parse", "HEAD"); head != res.Base {
				t.Errorf("HEAD = %s, want base %s", head, res.Base)
			}
			if _, err := os.Stat(filepath.Join(tr.Worktree, "out.txt")); err != nil {
				t.Errorf("the agent's file is gone: %v", err)
			}
			if mode == WorktreesSlots && !strings.Contains(tr.Error, "kept") {
				t.Errorf("error %q: a slot holding the work must be kept", tr.Error)
			}
		})
	}
}

func TestWorkLeftOffTheBranchDuringAContinuedAttemptKeepsTheEarlierCommit(t *testing.T) {
	for moveName, move := range map[string][]string{"detached": offMoves["detached"], "other branch": offMoves["other branch"]} {
		for _, mode := range []string{WorktreesFresh, WorktreesSlots} {
			t.Run(moveName+" "+mode, func(t *testing.T) {
				dir := initRepo(t)
				name := "wb-off-continued-" + strings.ReplaceAll(moveName, " ", "-") + "-" + mode
				calls := 0
				var earlier string
				agent.Register(stub{name, func(ctx context.Context, task agent.Task) agent.Result {
					calls++
					file := "first.txt"
					if calls > 1 {
						var err error
						earlier, err = gitOut(ctx, task.Dir, "rev-parse", "HEAD")
						if err != nil {
							return agent.Result{Status: agent.StatusError, Error: err.Error()}
						}
						if err := gitIn(task.Dir, move...); err != nil {
							return agent.Result{Status: agent.StatusError, Error: err.Error()}
						}
						file = "out.txt"
					}
					if err := os.WriteFile(filepath.Join(task.Dir, file), []byte("x\n"), 0o644); err != nil {
						return agent.Result{Status: agent.StatusError, Error: err.Error()}
					}
					return agent.Result{Backend: name, Status: agent.StatusOK, Turns: 1}
				}})
				res, _ := run(t, Spec{Dir: dir, Backend: name, Retry: []string{name, name}, Worktrees: mode,
					Gate: []string{"exit 3"}, Tasks: []Task{{ID: "off", Prompt: "p"}}})
				defer Remove(res)
				tr := res.Tasks[0]
				if tr.Status != StatusError || !strings.Contains(tr.Error, "not the task's branch") || len(tr.Attempts) != 2 || calls != 2 {
					t.Fatalf("task = %+v, calls = %d; want two attempts ending off the branch", tr, calls)
				}
				if tr.Attempts[0].Continued || !tr.Attempts[1].Continued || failedGate(tr.Attempts[0]) == nil {
					t.Errorf("attempts = %+v; want a gate failure followed by a continued attempt", tr.Attempts)
				}
				if earlier == "" || earlier == res.Base || tr.Commit != earlier {
					t.Errorf("commit = %q, earlier = %q, base = %q; want the earlier commit recorded", tr.Commit, earlier, res.Base)
				}
				if tip := git(t, dir, "rev-parse", "refs/heads/"+tr.Branch); tip != earlier {
					t.Errorf("task branch moved to %s, want %s", tip, earlier)
				}
				diff, untracked, err := Diff(context.Background(), res, tr.ID)
				if err != nil || !strings.Contains(diff, "diff --git a/first.txt b/first.txt") || strings.Contains(diff, "out.txt") || len(untracked) != 0 {
					t.Errorf("Diff = %q, %q, %v; want only the earlier committed file", diff, untracked, err)
				}
				if _, err := os.Stat(filepath.Join(tr.Worktree, "out.txt")); err != nil {
					t.Errorf("the agent's file is gone: %v", err)
				}
				merged, err := Merge(context.Background(), res, nil, false)
				if err == nil || !strings.Contains(err.Error(), "no task that passed") || len(merged) != 0 {
					t.Errorf("merged = %+v, %v; want nothing to merge", merged, err)
				}
				if mode == WorktreesSlots && !strings.Contains(tr.Error, "kept") {
					t.Errorf("error %q: a slot holding the work must be kept", tr.Error)
				}
			})
		}
	}
}

func TestDiffReadsARecordedBaseOnlyAsARevision(t *testing.T) {
	for name, move := range map[string][]string{"committed": nil, "orphan": offMoves["orphan"]} {
		t.Run(name, func(t *testing.T) {
			dir := initRepo(t)
			backend := offBranch("wb-base-option-"+name, move, agent.StatusOK)
			res, _ := run(t, Spec{Dir: dir, Backend: backend, Worktrees: WorktreesFresh, Tasks: []Task{{ID: "off", Prompt: "p"}}})
			defer Remove(res)
			if (res.Tasks[0].Commit != "") != (move == nil) {
				t.Fatalf("task = %+v; the committed case needs a commit and the orphan none", res.Tasks[0])
			}
			if _, _, err := Diff(context.Background(), res, "off"); err != nil {
				t.Fatalf("Diff with the real base: %v", err)
			}
			written := filepath.Join(t.TempDir(), "written")
			res.Base = "--output=" + written
			if _, _, err := Diff(context.Background(), res, "off"); err == nil {
				t.Error("Diff took an option for the base and succeeded")
			}
			// The committed path appends ..<commit>, so an option would write to that name.
			for _, path := range []string{written, written + ".." + res.Tasks[0].Commit} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("git diff read the recorded base as an option and wrote %s (%v)", path, err)
				}
			}
		})
	}
}

func TestDiffOfAWorktreeHoldingAFileNamedHEADReadsHEADAsARevision(t *testing.T) {
	dir := initRepo(t)
	backend := offBranch("wb-head-file", offMoves["detached"], agent.StatusOK)
	res, _ := run(t, Spec{Dir: dir, Backend: backend, Worktrees: WorktreesFresh, Tasks: []Task{{ID: "off", Prompt: "p"}}})
	defer Remove(res)
	if err := os.WriteFile(filepath.Join(res.Tasks[0].Worktree, "HEAD"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diff, untracked, err := Diff(context.Background(), res, "off")
	if err != nil || diff != "" || !reflect.DeepEqual(untracked, []string{"HEAD", "out.txt"}) {
		t.Errorf("Diff = %q, %q, %v; want no diff and the two untracked files", diff, untracked, err)
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
				tr := res.Tasks[0]
				where := "HEAD is on branch " + move[len(move)-1]
				if name == "detached" {
					where = "HEAD is detached"
				}
				if !strings.Contains(tr.Error, where+", not the task's branch "+tr.Branch) {
					t.Errorf("error = %q; want the exact HEAD location %q", tr.Error, where)
				}
				diff, untracked, err := Diff(context.Background(), res, tr.ID)
				var want []string
				if mode == WorktreesFresh {
					want = []string{"out.txt"}
				}
				if err != nil || diff != "" || !reflect.DeepEqual(untracked, want) {
					t.Errorf("Diff = %q, %q, %v; want no committed work and untracked %q", diff, untracked, err, want)
				}
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
	moves := map[string][]string{
		"detached":     offMoves["detached"],
		"other branch": offMoves["other branch"],
		// switch empties the index, so the tree is clean with no commit at HEAD.
		"empty orphan": {"switch", "-q", "--orphan", "agent-orphan"},
	}
	for name, move := range moves {
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
				tr := res.Tasks[0]
				// ws cannot count an unborn HEAD's unpushed commits, so it keeps that slot.
				keptOK := name == "empty orphan" && mode == WorktreesSlots
				if tr.Status != StatusPass || tr.Commit != "" || (!keptOK && strings.Contains(tr.Error, "kept")) {
					t.Errorf("status %s, commit %q, error %q: want a pass with nothing recorded and the slot given back", tr.Status, tr.Commit, tr.Error)
				}
			})
		}
	}
}

func TestAnAgentsOffBranchCommitEndsTheTaskAndKeepsItsWorktree(t *testing.T) {
	for _, moveName := range []string{"detached", "other branch"} {
		for _, mode := range []string{WorktreesFresh, WorktreesSlots} {
			for _, ending := range []string{"pass", "error", "gate", "canceled", "continued"} {
				t.Run(moveName+"/"+mode+"/"+ending, func(t *testing.T) {
					dir := initRepo(t)
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					name := "wb-off-commit"
					calls := 0
					var head, earlier string
					agent.Register(stub{name, func(ctx context.Context, task agent.Task) agent.Result {
						fail := func(err error) agent.Result {
							return agent.Result{Backend: name, Status: agent.StatusError, Error: err.Error()}
						}
						calls++
						var err error
						earlier, err = gitOut(ctx, task.Dir, "rev-parse", "HEAD")
						if err != nil {
							return fail(err)
						}
						if ending != "continued" || calls > 1 {
							if err := gitIn(task.Dir, offMoves[moveName]...); err != nil {
								return fail(err)
							}
						}
						file := "out.txt"
						if ending == "continued" && calls == 1 {
							file = "first.txt"
						}
						if err := os.WriteFile(filepath.Join(task.Dir, file), []byte("x\n"), 0o644); err != nil {
							return fail(err)
						}
						for _, args := range [][]string{{"add", file}, {"commit", "-q", "-m", "agent work"}} {
							if err := gitIn(task.Dir, args...); err != nil {
								return fail(err)
							}
						}
						head, err = gitOut(ctx, task.Dir, "rev-parse", "HEAD")
						if err != nil {
							return fail(err)
						}
						status := agent.StatusOK
						if ending == "error" {
							status = agent.StatusError
						}
						if ending == "canceled" {
							cancel()
							status = agent.StatusCanceled
						}
						return agent.Result{Backend: name, Status: status, Turns: 1}
					}})
					spec := Spec{Dir: dir, Backend: name, Retry: []string{name, name}, Worktrees: mode,
						Tasks: []Task{{ID: "off", Prompt: "p"}}}
					if ending == "gate" || ending == "continued" {
						spec.Gate = []string{"exit 3"}
					}
					res, err := Run(ctx, spec, nil)
					defer Remove(res)
					if err != nil && ending != "canceled" {
						t.Fatal(err)
					}
					tr := res.Tasks[0]
					wantStatus, attempts := StatusError, 1
					if ending == "canceled" {
						wantStatus = StatusCanceled
					}
					if ending == "continued" {
						attempts = 2
					}
					if tr.Status != wantStatus || calls != attempts || len(tr.Attempts) != attempts {
						t.Fatalf("status %s, calls %d, attempts %d: want %s and %d attempt(s)", tr.Status, calls, len(tr.Attempts), wantStatus, attempts)
					}
					if ending != "continued" && ending != "canceled" {
						assertLeftOff(t, dir, res, attempts)
					}
					wantCommit := ""
					if ending == "continued" {
						wantCommit = earlier
						if !tr.Attempts[1].Continued || earlier == res.Base {
							t.Error("the second attempt must continue an earlier commit")
						}
					}
					if tr.Commit != wantCommit || git(t, dir, "rev-parse", "refs/heads/"+tr.Branch) != earlier {
						t.Errorf("recorded commit %q, want %q, with the task branch unchanged", tr.Commit, wantCommit)
					}
					where := "HEAD is detached"
					if moveName == "other branch" {
						where = "HEAD is on branch agent-side"
					}
					for _, note := range []string{tr.Error, tr.Attempts[attempts-1].Error} {
						if !strings.Contains(note, where) || !strings.Contains(note, head[:7]) {
							t.Errorf("note %q must name %q and commit %s", note, where, head[:7])
						}
					}
					if got := git(t, tr.Worktree, "rev-parse", "HEAD"); got != head {
						t.Errorf("worktree HEAD = %s, want retained commit %s", got, head)
					}
					if got := git(t, tr.Worktree, "status", "--porcelain"); got != "" {
						t.Errorf("agent left a dirty tree: %q", got)
					}
					if mode == WorktreesSlots && !strings.Contains(tr.Error, "kept") {
						t.Errorf("slot was not kept: %q", tr.Error)
					}
					if merged, err := Merge(context.Background(), res, nil, false); err == nil || len(merged) != 0 {
						t.Errorf("Merge = %+v, %v: want refusal", merged, err)
					}
				})
			}
		}
	}
}

func TestAnAgentMovingOffTheBranchAndBackStillPasses(t *testing.T) {
	for _, moveName := range []string{"detached", "other branch"} {
		for _, mode := range []string{WorktreesFresh, WorktreesSlots} {
			t.Run(moveName+"/"+mode, func(t *testing.T) {
				dir := initRepo(t)
				name := "wb-off-back"
				agent.Register(stub{name, func(ctx context.Context, task agent.Task) agent.Result {
					branch, err := gitOut(ctx, task.Dir, "branch", "--show-current")
					if err != nil {
						return agent.Result{Status: agent.StatusError, Error: err.Error()}
					}
					for _, args := range [][]string{offMoves[moveName], {"checkout", "-q", branch}} {
						if err := gitIn(task.Dir, args...); err != nil {
							return agent.Result{Status: agent.StatusError, Error: err.Error()}
						}
					}
					return agent.Result{Backend: name, Status: agent.StatusOK, Turns: 1}
				}})
				res, _ := run(t, Spec{Dir: dir, Backend: name, Worktrees: mode, Tasks: []Task{{ID: "back", Prompt: "p"}}})
				defer Remove(res)
				if tr := res.Tasks[0]; tr.Status != StatusPass || tr.Commit != "" || tr.Error != "" || tr.Attempts[0].Error != "" {
					t.Errorf("task = %+v: want a pass with no commit or note", tr)
				}
			})
		}
	}
}
