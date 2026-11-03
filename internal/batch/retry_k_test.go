package batch

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRetryKValidation(t *testing.T) {
	for _, n := range []int{-1, 0, 1, 2, 5, 6} {
		for _, task := range []bool{false, true} {
			s := Spec{Tasks: []Task{{Prompt: "work"}}}
			if task {
				s.Tasks[0].RetryK = n
			} else {
				s.RetryK = n
			}
			err := s.normalize()
			valid := n == 0 || n >= 2 && n <= 5
			if (err == nil) != valid {
				t.Errorf("retry_k=%d task=%v: %v", n, task, err)
			}
		}
	}
}

func TestPickRetryWinner(t *testing.T) {
	pass := []GateResult{{ExitCode: 0}}
	fail := []GateResult{{ExitCode: 1}}
	for _, tc := range []struct {
		name       string
		candidates []RetryCandidate
		want       int
	}{
		{"winner", []RetryCandidate{{Gates: fail}, {Gates: pass, Tokens: 9}}, 1},
		{"none", []RetryCandidate{{Gates: fail}, {Gates: fail}}, -1},
		{"lower tokens", []RetryCandidate{{Gates: pass, Tokens: 9}, {Gates: pass, Tokens: 4}}, 1},
		{"equal tokens keeps proposal order", []RetryCandidate{{Gates: pass, Tokens: 4}, {Gates: pass, Tokens: 4}}, 0},
		{"no gates", []RetryCandidate{{}}, 0},
		{"infrastructure failure", []RetryCandidate{{Gates: []GateResult{{InfrastructureError: "failed"}}}}, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickRetryWinner(tc.candidates); got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRetryCandidateRecords(t *testing.T) {
	var old Result
	if err := json.Unmarshal([]byte(`{"id":"old","tasks":[{"id":"a","status":"pass","attempts":[]}]}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.ID != "old" || len(old.Tasks) != 1 || old.Tasks[0].Candidates != nil {
		t.Fatalf("old record: %+v", old)
	}
	old.Tasks[0].Candidates = []RetryCandidate{{BriefAddition: "try", RunID: "run", Gates: []GateResult{{ExitCode: 0}}, Tokens: 42, Winner: true}}
	data, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	var back Result
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	c := back.Tasks[0].Candidates[0]
	if c.BriefAddition != "try" || c.RunID != "run" || len(c.Gates) != 1 || c.Tokens != 42 || !c.Winner {
		t.Fatalf("candidate: %+v", c)
	}
	old.Tasks[0].Candidates = nil
	data, _ = json.Marshal(old)
	if strings.Contains(string(data), "candidates") {
		t.Fatal("empty candidates must be omitted")
	}
}
