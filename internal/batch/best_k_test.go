package batch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
)

var bestKMu sync.Mutex
var bestKActive, bestKPeak int

func init() {
	agent.Register(stub{"wb-best-k", func(_ context.Context, task agent.Task) agent.Result {
		if task.Purpose == "plan" {
			switch {
			case strings.Contains(task.Prompt, "BRACKET_TEST"):
				return agent.Result{Status: agent.StatusOK, Final: `Based on the failure [exit 1], here are: ["one", "two"]`}
			case strings.Contains(task.Prompt, "CAP_TEST"):
				return agent.Result{Status: agent.StatusOK, Final: `["one", "two", "three", "four", "five"]`}
			case strings.Contains(task.Prompt, "PROPOSER_FAIL"):
				return agent.Result{Status: agent.StatusError, Error: "proposal failed"}
			case strings.Contains(task.Prompt, "NO_WIN"):
				return agent.Result{Status: agent.StatusOK, Final: `["one", "two", "three"]`}
			case strings.Contains(task.Prompt, "TIE"):
				return agent.Result{Status: agent.StatusOK, Final: `["PASS expensive", "PASS cheap", "three"]`}
			default:
				return agent.Result{Status: agent.StatusOK, Final: `["one", "PASS cheap", "three"]`}
			}
		}
		if strings.Contains(task.Prompt, "CAP_TEST\n\n") {
			bestKMu.Lock()
			bestKActive++
			if bestKActive > bestKPeak {
				bestKPeak = bestKActive
			}
			bestKMu.Unlock()
			time.Sleep(3 * time.Second)
			bestKMu.Lock()
			bestKActive--
			bestKMu.Unlock()
		}
		n := int64(10)
		if strings.Contains(task.Prompt, "PASS") {
			_ = os.WriteFile(filepath.Join(task.Dir, "pass.txt"), []byte("pass"), 0644)
		}
		if strings.Contains(task.Prompt, "expensive") {
			n = 50
		}
		return agent.Result{Status: agent.StatusOK, Final: "done", InputTokens: n}
	}})
}

func TestBestOfKExecution(t *testing.T) {
	for _, tc := range []struct {
		name, prompt, mode string
		winner             int
		proposalFail       bool
	}{
		{"winner", "work", WorktreesFresh, 2, false},
		{"none", "NO_WIN", WorktreesFresh, 0, false},
		{"tokens", "TIE", WorktreesFresh, 2, false},
		{"proposer", "PROPOSER_FAIL", WorktreesFresh, 0, true},
		{"slots", "work", WorktreesSlots, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := initRepo(t)
			res, err := Run(context.Background(), Spec{Dir: dir, Backend: "wb-best-k", Worktrees: tc.mode, RetryK: 3, Gate: []string{"test -f pass.txt"}, Tasks: []Task{{ID: "best", Prompt: tc.prompt}}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			tr := res.Tasks[0]
			if tc.proposalFail {
				if tr.Status != StatusFail || !strings.Contains(tr.Error, "proposal failed") {
					t.Fatalf("proposal failure: %+v", tr)
				}
				return
			}
			if len(tr.Candidates) != 3 {
				t.Fatalf("candidates: %+v", tr)
			}
			if tc.winner == 0 && tr.Status != StatusFail || tc.winner != 0 && (tr.Status != StatusPass || tr.Branch != fmt.Sprintf("hrn/best-k%d", tc.winner)) {
				t.Errorf("result: %+v", tr)
			}
			original := git(t, dir, "for-each-ref", "--format=%(refname)", "refs/heads/hrn/best") != ""
			if original != (tc.winner == 0) {
				t.Errorf("original branch exists = %v", original)
			}
			for i, c := range tr.Candidates {
				if c.Winner != (i+1 == tc.winner) || c.RunID == "" || c.BriefAddition == "" || len(c.Gates) == 0 || c.Tokens == 0 {
					t.Errorf("candidate %d: %+v", i, c)
				}
				cmd := fmt.Sprintf("refs/heads/hrn/best-k%d", i+1)
				exists := git(t, dir, "for-each-ref", "--format=%(refname)", cmd) != ""
				if exists != (i+1 == tc.winner) {
					t.Errorf("branch %s exists = %v", cmd, exists)
				}
			}
		})
	}
}

func TestBestOfKRespectsSlotBound(t *testing.T) {
	dir := initRepo(t)
	bestKMu.Lock()
	bestKActive, bestKPeak = 0, 0
	bestKMu.Unlock()
	res, err := Run(context.Background(), Spec{Dir: dir, Backend: "wb-best-k", Parallel: 2, Worktrees: WorktreesSlots, RetryK: 5, Gate: []string{"test -f pass.txt"}, Tasks: []Task{{ID: "one", Prompt: "CAP_TEST"}, {ID: "two", Prompt: "CAP_TEST"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range res.Tasks {
		if len(tr.Candidates) != 5 || tr.Status != StatusFail {
			t.Errorf("result: %+v", tr)
		}
	}
	bestKMu.Lock()
	peak := bestKPeak
	bestKMu.Unlock()
	if peak < 2 || peak > 5 {
		t.Errorf("peak candidate activity = %d, want 2..5", peak)
	}
}

func TestProposalSkipsNonJSONBrackets(t *testing.T) {
	got, err := proposeAdditions(context.Background(), "wb-best-k", Task{Prompt: "BRACKET_TEST"}, TaskResult{}, 2, t.TempDir(), agent.Options{})
	if err != nil || len(got) != 2 {
		t.Fatalf("additions %v: %v", got, err)
	}
}

func TestRetryKTaskIDCollision(t *testing.T) {
	for _, tasks := range [][]Task{
		{{ID: "x", Prompt: "x", RetryK: 2}, {ID: "x-k1", Prompt: "y"}},
		{{ID: "x-k12", Prompt: "y"}, {ID: "x", Prompt: "x"}},
	} {
		s := Spec{RetryK: 2, Tasks: tasks}
		if err := s.normalize(); err == nil {
			t.Errorf("accepted collision: %+v", tasks)
		}
	}
}

func TestCandidateClaimCleanup(t *testing.T) {
	dir := initRepo(t)
	git(t, dir, "branch", "hrn/candidate")
	res := Result{Dir: dir, Ownership: OwnershipClaim, Tasks: []TaskResult{{ID: "parent", Status: StatusRunning, Candidates: []RetryCandidate{{Branch: "hrn/candidate", BranchMade: true}}}}}
	if !WorktreesRemain(res) {
		t.Fatal("candidate claim not detected")
	}
	if err := Remove(res); err != nil {
		t.Fatal(err)
	}
	if WorktreesRemain(res) {
		t.Fatal("candidate claim remains")
	}
}

func TestBestOfKClaimsAndCancellation(t *testing.T) {
	dir := initRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	claimed := map[string]bool{}
	var canceledDecision bool
	agent.Register(stub{"wb-k-cancel", func(ctx context.Context, task agent.Task) agent.Result {
		if task.Purpose == "plan" {
			return agent.Result{Status: agent.StatusOK, Final: `["one","two"]`}
		}
		if strings.Contains(task.Prompt, "work\n\none") || strings.Contains(task.Prompt, "work\n\ntwo") {
			<-ctx.Done()
			return agent.Result{Status: agent.StatusCanceled, Error: ctx.Err().Error()}
		}
		return agent.Result{Status: agent.StatusOK, Final: "done"}
	}})
	failSave = func(res Result) error {
		mu.Lock()
		defer mu.Unlock()
		for _, tr := range res.Tasks {
			for _, c := range tr.Candidates {
				if c.BranchMade {
					claimed[c.Branch] = true
				}
			}
		}
		if len(claimed) == 2 {
			cancel()
		}
		return nil
	}
	defer func() { failSave = nil }()
	res, err := Run(ctx, Spec{Dir: dir, Backend: "wb-k-cancel", Worktrees: WorktreesFresh, RetryK: 2, Gate: []string{"false"}, Tasks: []Task{{ID: "cancel", Prompt: "work"}}}, func(e Event) {
		if e.Kind == "decision" && strings.Contains(e.Text, "best-of-k: canceled") {
			mu.Lock()
			canceledDecision = true
			mu.Unlock()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(claimed) != 2 || !claimed["hrn/cancel-k1"] || !claimed["hrn/cancel-k2"] {
		t.Errorf("claims: %v", claimed)
	}
	if res.Tasks[0].Status != StatusCanceled || !canceledDecision {
		t.Errorf("result=%+v decision=%v", res.Tasks[0], canceledDecision)
	}
}
