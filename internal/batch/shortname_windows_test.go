//go:build windows

package batch

import (
	"context"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/followup"
)

// shortName is long's 8.3 name. The test skips on a volume that makes none.
func shortName(t *testing.T, long string) string {
	t.Helper()
	p, err := syscall.UTF16PtrFromString(long)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, 1024)
	n, err := syscall.GetShortPathName(p, &buf[0], uint32(len(buf)))
	if err != nil || n == 0 || int(n) > len(buf) {
		t.Skipf("no short name for %s: %v", long, err)
	}
	short := syscall.UTF16ToString(buf[:n])
	if strings.EqualFold(filepath.Base(short), filepath.Base(long)) {
		t.Skipf("this volume makes no 8.3 names (%s)", short)
	}
	return short
}

type forkingStub struct{ stub }

func (forkingStub) ForksSessions() bool { return true }

func TestABatchReachedThroughAShortNameRecordsALongDirectoryAndCanBeContinued(t *testing.T) {
	const backend = "wb-short-session"
	agent.Register(forkingStub{stub{backend, func(context.Context, agent.Task) agent.Result {
		return agent.Result{Backend: backend, Status: agent.StatusOK, Session: "batch-session"}
	}}})
	for _, mode := range []string{WorktreesFresh, WorktreesSlots} {
		t.Run(mode, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(initRepo(t))
			if err != nil {
				t.Fatal(err)
			}
			// TempDir's numeric leaf needs no alias; its long parent does.
			short := filepath.Join(shortName(t, filepath.Dir(dir)), filepath.Base(dir))
			resolved, err := filepath.EvalSymlinks(short)
			if err != nil || resolved != dir {
				t.Fatalf("short name resolves to %q, want %q: %v", resolved, dir, err)
			}
			res, _ := run(t, Spec{Dir: short, Worktrees: mode, Backend: backend,
				Tasks: []Task{{ID: "short-path", Prompt: "nothing to change"}}})
			if len(res.Tasks) != 1 || res.Tasks[0].Status != StatusPass {
				t.Fatalf("batch failed: %+v", res)
			}
			rec := firstRun(t, res.Tasks[0])
			long, err := filepath.EvalSymlinks(res.Tasks[0].Worktree)
			if err != nil {
				t.Fatal(err)
			}
			// Compare the recorded text itself: resolving both sides would hide a regression.
			if rec.Task.Dir != long {
				t.Fatalf("recorded directory = %q, want long spelling %q", rec.Task.Dir, long)
			}
			plan, err := followup.Make(followup.Request{RunID: rec.ID, Dir: long})
			if err != nil {
				t.Fatal(err)
			}
			if plan.Dir != long || plan.ForkSession != "batch-session" || len(plan.Handoff) != 0 {
				t.Fatalf("batch session was not continued: %+v", plan)
			}
		})
	}
}
