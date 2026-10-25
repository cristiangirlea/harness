package batch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/flow"
	"github.com/cristiangirlea/harness/internal/tools"
)

type startupStub struct{ stub }

func (s startupStub) Run(_ context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
	r := agent.Result{Backend: s.Name(), Status: agent.StatusError, Error: "launch refused\r\nmore detail"}
	prompt := typed(task)
	switch prompt {
	case "turn", "gate-turn":
		r.Turns = 1
	case "tools", "gate-tools":
		emit(agent.Event{Kind: agent.EventToolCall, Tool: "read_file", Text: "README.md"})
	case "timeout":
		r.Status = agent.StatusTimeout
	case "canceled":
		r.Status = agent.StatusCanceled
	case "limited":
		r.Status = agent.StatusLimited
	case "empty-error":
		r.Error = ""
	case "long":
		r.Duration = time.Hour
	case "tokens":
		r.OutputTokens = 12 // an adapter that reports no turns still proves the model answered
	}
	if strings.HasPrefix(prompt, "gate") || prompt == "ok" {
		r.Status, r.Error = agent.StatusOK, ""
	}
	return r
}

func init() { agent.Register(startupStub{stub{name: "wb-startup"}}) }

func TestAttemptDidNotStartRequiresEvidence(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, tc := range []struct {
		prompt string
		want   string
	}{
		{"no-start", "launch refused"},
		{"long", "launch refused"},
		{"turn", ""},
		{"tools", ""},
		{"timeout", ""},
		{"canceled", ""},
		{"limited", ""},
		{"empty-error", ""},
		{"ok", ""},
		{"gate", ""},
		{"tokens", ""},
		{"gate-turn", ""},
		{"gate-tools", ""},
		{"gate-exit", ""},
	} {
		t.Run(tc.prompt, func(t *testing.T) {
			dir := initRepo(t)
			var gates []string
			if strings.HasPrefix(tc.prompt, "gate") {
				if tc.prompt == "gate-exit" {
					fakeGate(t, tools.ShellResult{ExitCode: 1}, nil)
				} else {
					fakeGate(t, tools.ShellResult{}, errors.New("launcher unavailable\nmore detail"))
				}
				gates = []string{"check"}
			}
			res, _ := run(t, Spec{Dir: dir, Backend: "wb-startup", Worktrees: "fresh", Gate: gates,
				Tasks: []Task{{ID: "task", Prompt: tc.prompt}}})
			loaded, err := LoadResult(res.ID)
			if err != nil {
				t.Fatal(err)
			}
			att := loaded.Tasks[0].Attempts[0]
			if len(att.Gates) != len(gates) {
				t.Fatalf("gates = %+v, want %d gates", att.Gates, len(gates))
			}
			if att.DidNotStart != tc.want {
				t.Errorf("did_not_start = %q, want %q", att.DidNotStart, tc.want)
			}
			data, err := json.Marshal(att)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), `"did_not_start"`) != (tc.want != "") {
				t.Errorf("did_not_start presence in %s", data)
			}
			if tc.want == "" {
				return
			}
			live, err := flow.Read(FlowPath(res.ID))
			if err != nil {
				t.Fatal(err)
			}
			for name, events := range map[string][]flow.Event{"live": live, "rebuilt": FlowOf(loaded, nil)} {
				node := find(flow.Build(events), AttemptStep("task", 1))
				if node == nil || node.Status != flow.StatusFailed || node.Reason != "backend did not start: "+tc.want {
					t.Errorf("%s attempt = %+v", name, node)
				}
			}
		})
	}
}

func TestFlowOfDidNotStartOverridesRunStatus(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	res := Result{Started: time.Now(), Tasks: []TaskResult{{ID: "task", Status: StatusFail, Attempts: []Attempt{{
		DidNotStart: "launch refused", Result: agent.Result{Status: agent.StatusOK},
	}}}}}
	node := find(flow.Build(FlowOf(res, nil)), AttemptStep("task", 1))
	if node == nil || node.Status != flow.StatusFailed || node.Reason != "backend did not start: launch refused" {
		t.Fatalf("attempt flow = %+v", node)
	}
}

func TestOldAttemptDoesNotGainDidNotStart(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	var att Attempt
	if err := json.Unmarshal([]byte(`{"backend":"old","result":{"status":"error","error":"launch refused","turns":0}}`), &att); err != nil {
		t.Fatal(err)
	}
	if att.DidNotStart != "" {
		t.Fatalf("old attempt = %+v", att)
	}
	res := Result{Started: time.Now(), Tasks: []TaskResult{{ID: "task", Status: StatusFail, Attempts: []Attempt{att}}}}
	node := find(flow.Build(FlowOf(res, nil)), AttemptStep("task", 1))
	if node == nil || node.Status != flow.StatusFailed || strings.Contains(node.Reason, "backend did not start") {
		t.Fatalf("old attempt flow = %+v", node)
	}
}
