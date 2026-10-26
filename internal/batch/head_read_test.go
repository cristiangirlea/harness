package batch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
)

func init() {
	if os.Getenv("HRN_TEST_HEAD_READ_GIT") != "" && strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") == "git" {
		os.Args = append([]string{os.Args[0], "-test.run=^TestHelperOffBranchGit$", "--"}, os.Args[1:]...)
	}
}

func TestHelperOffBranchGit(t *testing.T) {
	realGit := os.Getenv("HRN_TEST_HEAD_READ_GIT")
	if realGit == "" {
		return
	}
	args := os.Args[slices.Index(os.Args, "--")+1:]
	if _, err := os.Stat(os.Getenv("HRN_TEST_HEAD_READ_MARKER")); err == nil &&
		len(args) >= 4 && slices.Equal(args[len(args)-4:], []string{"rev-parse", "-q", "--verify", "HEAD"}) {
		fmt.Fprintln(os.Stderr, "injected HEAD read failure")
		os.Exit(1)
	}
	cmd := exec.Command(realGit, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			os.Exit(e.ExitCode())
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestOffBranchHEADReadFailureEndsWithoutRetry(t *testing.T) {
	for _, move := range []string{"detached", "other branch"} {
		for _, mode := range []string{WorktreesFresh, WorktreesSlots} {
			t.Run(move+"/"+mode, func(t *testing.T) {
				dir := initRepo(t)
				realGit, err := exec.LookPath("git")
				if err != nil {
					t.Fatal(err)
				}
				bin := t.TempDir()
				executable, err := os.ReadFile(os.Args[0])
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(bin, "git"+filepath.Ext(realGit)), executable, 0o755); err != nil {
					t.Fatal(err)
				}
				marker := filepath.Join(t.TempDir(), "fail-head-read")
				t.Setenv("HRN_TEST_HEAD_READ_GIT", realGit)
				t.Setenv("HRN_TEST_HEAD_READ_MARKER", marker)
				t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
				name := "wb-head-read-error"
				calls := 0
				agent.Register(stub{name, func(ctx context.Context, task agent.Task) agent.Result {
					calls++
					fail := func(err error) agent.Result {
						return agent.Result{Status: agent.StatusError, Error: err.Error()}
					}
					for _, args := range [][]string{offMoves[move], {"commit", "--allow-empty", "-q", "-m", "agent work"}} {
						if err := gitIn(task.Dir, args...); err != nil {
							return fail(err)
						}
					}
					if err := os.WriteFile(marker, nil, 0o600); err != nil {
						return fail(err)
					}
					status, err := gitOut(ctx, task.Dir, "status", "--porcelain")
					if err != nil || status != "" {
						return fail(fmt.Errorf("status = %q, error = %v", status, err))
					}
					head, err := gitOut(ctx, task.Dir, "branch", "--show-current")
					wantHead := ""
					if move == "other branch" {
						wantHead = "agent-side"
					}
					if err != nil || head != wantHead {
						return fail(fmt.Errorf("branch = %q, error = %v", head, err))
					}
					return agent.Result{Backend: name, Status: agent.StatusOK, Turns: 1}
				}})
				res, _ := run(t, Spec{Dir: dir, Backend: name, Retry: []string{name, name}, Worktrees: mode,
					Tasks: []Task{{ID: "off", Prompt: "p"}}})
				defer Remove(res)
				tr := res.Tasks[0]
				if len(tr.Attempts) != 1 || calls != 1 {
					t.Fatalf("calls = %d, attempts = %d; want one attempt", calls, len(tr.Attempts))
				}
				if tr.Attempts[0].Result.Status != agent.StatusOK {
					t.Fatalf("stub failed before commitChanges: %+v", tr.Attempts[0].Result)
				}
				if tr.Status != StatusError || tr.Commit != "" {
					t.Errorf("status = %s, commit = %q; want error without a recorded commit", tr.Status, tr.Commit)
				}
				for _, msg := range []string{tr.Error, tr.Attempts[0].Error} {
					if !strings.Contains(msg, "rev-parse -q --verify HEAD: injected HEAD read failure") ||
						strings.Contains(msg, "not the task's branch") {
						t.Errorf("error = %q; want the git HEAD read error", msg)
					}
				}
			})
		}
	}
}
