package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/tools"
)

type checkpointStore struct {
	latest         *APICheckpoint
	records        []APICheckpoint
	failBoundary   string
	commitThenFail bool
}

func (s *checkpointStore) save(_ context.Context, cp APICheckpoint) error {
	fail := cp.Boundary == s.failBoundary
	if fail && !s.commitThenFail {
		return errors.New("injected persistence failure")
	}
	data, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	var clone APICheckpoint
	if err := json.Unmarshal(data, &clone); err != nil {
		return err
	}
	if cp.Sequence != uint64(len(s.records)+1) {
		return errors.New("invalid checkpoint sequence")
	}
	s.records = append(s.records, clone)
	s.latest = &clone
	if fail {
		return errors.New("injected lost acknowledgement")
	}
	return nil
}

func durableFixture(t *testing.T, store *checkpointStore) (*APIBackend, Task, *fakeAPI, *int) {
	t.Helper()
	fake := &fakeAPI{turns: [][]string{toolTurn("effect-1", "effect", `{}`), textTurn("done")}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	effects := new(int)
	backend := NewAPIBackend()
	backend.BaseURL, backend.APIKey = server.URL, "test"
	backend.Tools = []tools.Tool{{Name: "effect", Effect: tools.EffectWrite, Run: func(context.Context, *tools.Workspace, json.RawMessage) (string, error) {
		*effects++
		return "changed", nil
	}}}
	task := Task{Prompt: "test", Dir: t.TempDir(), Options: Options{Mode: ModeYolo, MaxTurns: 3, Timeout: Duration(time.Minute)}, Durable: &DurableExecution{Save: store.save}}
	return backend, task, fake, effects
}

func TestDurableStopsAtFailedSavesAndResumesOnlyConfirmedOperations(t *testing.T) {
	tests := []struct {
		boundary      string
		committed     bool
		firstRequests int
		firstEffects  int
		resumable     bool
	}{
		{"model_intent", false, 0, 0, false},
		{"model_intent", true, 0, 0, false},
		{"model_response", false, 1, 0, false},
		{"model_response", true, 1, 0, true},
		{"tool_intent", false, 1, 0, true},
		{"tool_intent", true, 1, 0, false},
		{"tool_outcome", false, 1, 1, false},
		{"tool_outcome", true, 1, 1, true},
		{"turn_complete", false, 1, 1, true},
		{"turn_complete", true, 1, 1, true},
		{"finished", false, 2, 1, true},
		{"finished", true, 2, 1, true},
	}
	for _, tc := range tests {
		name := tc.boundary + "/before_commit"
		if tc.committed {
			name = tc.boundary + "/lost_ack"
		}
		t.Run(name, func(t *testing.T) {
			store := &checkpointStore{failBoundary: tc.boundary, commitThenFail: tc.committed}
			backend, task, fake, effects := durableFixture(t, store)
			first := Run(context.Background(), backend, task, func(Event) {})
			if first.Status != StatusError || !first.RecoveryRequired {
				t.Fatalf("failure presented as resumable completion: %+v", first)
			}
			if fake.served != tc.firstRequests || *effects != tc.firstEffects {
				t.Fatalf("operations crossed failed boundary: requests=%d effects=%d", fake.served, *effects)
			}
			if store.latest == nil {
				return
			} // Nothing was ever acknowledged.
			checkpoint := store.latest
			store.failBoundary = ""
			task.Durable.Resume = checkpoint
			second := Run(context.Background(), backend, task, func(Event) {})
			if !tc.resumable {
				if second.Status != StatusError || !second.RecoveryRequired || fake.served != tc.firstRequests || *effects != tc.firstEffects {
					t.Fatalf("uncertain operation was replayed: result=%+v requests=%d effects=%d", second, fake.served, *effects)
				}
				return
			}
			if second.Status != StatusOK || second.RecoveryRequired || second.Final != "done" || *effects != 1 || fake.served != 2 {
				t.Fatalf("confirmed operations repeated or task not completed: result=%+v requests=%d effects=%d", second, fake.served, *effects)
			}
			if second.InputTokens != 200 || second.OutputTokens != 40 || second.ContextTokens != 100 || !second.ModelReported || second.Turns != 2 || second.ToolCalls != 1 {
				t.Fatalf("cumulative usage changed across recovery: %+v", second)
			}
			if !store.latest.Deadline.Equal(checkpoint.Deadline) || !store.latest.Started.Equal(checkpoint.Started) {
				t.Fatal("recovery reset the task's clock")
			}
			// The restored response appears exactly once in the next request.
			msgs := fake.requests[1]["messages"].([]any)
			if len(msgs) != 3 {
				t.Fatalf("history was rewritten or duplicated: %v", msgs)
			}
		})
	}
}

func TestDurableResumeRejectsChangedTask(t *testing.T) {
	store := &checkpointStore{failBoundary: "model_response", commitThenFail: true}
	backend, task, fake, effects := durableFixture(t, store)
	backend.Run(context.Background(), task, func(Event) {})
	task.Durable.Resume = store.latest
	task.Mode = ModeReadOnly
	result := backend.Run(context.Background(), task, func(Event) {})
	if result.Status != StatusError || !strings.Contains(result.Error, "identity") || fake.served != 1 || *effects != 0 {
		t.Fatalf("changed permissions accepted: %+v", result)
	}
}

func TestDurableRefusesConversationContinuationBeforeRequestingAModelResponse(t *testing.T) {
	store := &checkpointStore{}
	backend, task, fake, effects := durableFixture(t, store)
	task.Resume = &Conversation{}
	result := backend.Run(context.Background(), task, func(Event) {})
	if result.Status != StatusError || !strings.Contains(result.Error, "cannot be combined") || fake.served != 0 || *effects != 0 || len(store.records) != 0 {
		t.Fatalf("conversation continuation was accepted with durable execution: result=%+v requests=%d effects=%d checkpoints=%d", result, fake.served, *effects, len(store.records))
	}
}

func TestDurableResumeRefusesInvalidToolOutcomesBeforeRunningTools(t *testing.T) {
	for _, tc := range []struct {
		name     string
		outcomes []map[string]any
		want     string
	}{
		{"excess outcomes", []map[string]any{{"tool_use_id": "effect-1"}, {"tool_use_id": "extra"}}, "excess tool outcomes"},
		{"mismatched ID", []map[string]any{{"tool_use_id": "wrong"}}, "tool outcome ID mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &checkpointStore{failBoundary: "model_response", commitThenFail: true}
			backend, task, fake, effects := durableFixture(t, store)
			first := backend.Run(context.Background(), task, func(Event) {})
			if !first.RecoveryRequired || store.latest == nil || fake.served != 1 {
				t.Fatalf("response was not checkpointed: result=%+v requests=%d", first, fake.served)
			}
			store.latest.ToolResults = tc.outcomes
			task.Durable.Resume = store.latest
			store.failBoundary = ""
			result := backend.Run(context.Background(), task, func(Event) {})
			if result.Status != StatusError || !strings.Contains(result.Error, tc.want) || fake.served != 1 || *effects != 0 {
				t.Fatalf("invalid outcome started execution: result=%+v requests=%d effects=%d", result, fake.served, *effects)
			}
		})
	}
}

func TestDurableResumeKeepsDeadline(t *testing.T) {
	store := &checkpointStore{failBoundary: "turn_complete", commitThenFail: true}
	backend, task, fake, effects := durableFixture(t, store)
	backend.Run(context.Background(), task, func(Event) {})
	store.latest.Deadline = time.Now().Add(-time.Second)
	task.Durable.Resume = store.latest
	store.failBoundary = ""
	result := backend.Run(context.Background(), task, func(Event) {})
	if result.Status != StatusTimeout || fake.served != 1 || *effects != 1 {
		t.Fatalf("deadline restarted: %+v", result)
	}
}

func TestDurableModelRequestIsNotAutomaticallyRetried(t *testing.T) {
	store := &checkpointStore{}
	backend, task, fake, _ := durableFixture(t, store)
	fake.turns = [][]string{nil, textTurn("unexpected retry")}
	result := backend.Run(context.Background(), task, func(Event) {})
	if result.Status != StatusError || !result.RecoveryRequired || fake.served != 1 || store.latest.Pending == nil {
		t.Fatalf("uncertain request retried: %+v requests=%d", result, fake.served)
	}
}

func TestDurableFailedStreamKeepsPartialUsage(t *testing.T) {
	for _, durable := range []bool{false, true} {
		name := "local"
		if durable {
			name = "durable"
		}
		t.Run(name, func(t *testing.T) {
			store := &checkpointStore{}
			backend, task, fake, _ := durableFixture(t, store)
			backend.MaxRetries = 0
			turn := textTurn("partial")
			fake.turns = [][]string{append(turn[:len(turn)-1],
				`{"type":"error","error":{"type":"api_error","message":"stream interrupted"}}`)}
			if !durable {
				task.Durable = nil
			}
			result := backend.Run(context.Background(), task, func(Event) {})
			if result.Status != StatusError || result.RecoveryRequired != durable || fake.served != 1 ||
				!strings.Contains(result.Error, "api stream api_error: stream interrupted") {
				t.Fatalf("failed stream result = %+v, requests = %d", result, fake.served)
			}
			wantCost := estimateCost("claude-opus-5", Result{InputTokens: 100, OutputTokens: 20})
			if wantCost <= 0 {
				t.Fatal("test model must have a price")
			}
			if result.InputTokens != 100 || result.OutputTokens != 20 || result.ContextTokens != 100 ||
				result.CostUSD != wantCost {
				t.Fatalf("partial usage lost: %+v", result)
			}
			if durable && (store.latest == nil || store.latest.Pending == nil || store.latest.Pending.Kind != "model") {
				t.Fatalf("failed stream lost pending model operation: %+v", store.latest)
			}
		})
	}
}

func TestDurableDoesNotStartSecondEffectAfterFailedOutcome(t *testing.T) {
	store := &checkpointStore{failBoundary: "tool_outcome"}
	effects := 0
	backend, task, _, _ := durableFixture(t, store)
	backend.Tools = []tools.Tool{{Name: "effect", Effect: tools.EffectWrite, Run: func(context.Context, *tools.Workspace, json.RawMessage) (string, error) {
		effects++
		return "ok", nil
	}}}
	req := apiRequest{Model: "test"}
	result := Result{}
	runtime, err := prepareDurable(task, &req, &result)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := tools.NewWorkspace(task.Dir)
	if err != nil {
		t.Fatal(err)
	}
	calls := []block{{"id": "a", "name": "effect", "input": block{}}, {"id": "b", "name": "effect", "input": block{}}}
	_, err = runtime.runTools(context.Background(), ws, task, calls, nil, map[string]tools.Tool{"effect": backend.Tools[0]}, func(Event) {}, req, &result)
	if !errors.Is(err, ErrRecoveryRequired) || effects != 1 {
		t.Fatalf("second effect crossed failed save: err=%v effects=%d", err, effects)
	}
}

func TestDurableSnapshotDoesNotAliasLoopState(t *testing.T) {
	store := &checkpointStore{}
	backend, task, _, _ := durableFixture(t, store)
	result := backend.Run(context.Background(), task, func(Event) {})
	if result.Status != StatusOK {
		t.Fatal(result)
	}
	for _, cp := range store.records {
		if cp.Boundary == "model_intent" && cp.NextTurn == 1 && (len(cp.Response) != 0 || len(cp.ToolResults) != 0 || cp.Pending == nil) {
			t.Fatalf("earlier snapshot changed: %+v", cp)
		}
	}
}

func TestDurableConfigurationIsNotAcceptedFromJSON(t *testing.T) {
	var task Task
	if err := json.Unmarshal([]byte(`{"prompt":"hello","Durable":{"Resume":{"version":1}}}`), &task); err != nil {
		t.Fatal(err)
	}
	if task.Durable != nil {
		t.Fatal("untrusted task selected its own persistence boundary")
	}
}

func TestDurableRejectsAmbiguousOperationIDsBeforeEffects(t *testing.T) {
	for _, badID := range []string{"", "a"} {
		t.Run("id="+badID, func(t *testing.T) {
			store := &checkpointStore{}
			backend, task, _, effects := durableFixture(t, store)
			req, result := apiRequest{}, Result{}
			runtime, err := prepareDurable(task, &req, &result)
			if err != nil {
				t.Fatal(err)
			}
			ws, err := tools.NewWorkspace(task.Dir)
			if err != nil {
				t.Fatal(err)
			}
			calls := []block{{"id": "a", "name": "effect", "input": block{}}, {"id": badID, "name": "effect", "input": block{}}}
			_, err = runtime.runTools(context.Background(), ws, task, calls, nil, map[string]tools.Tool{"effect": backend.Tools[0]}, func(Event) {}, req, &result)
			if err == nil || *effects != 0 || len(store.records) != 0 {
				t.Fatalf("ambiguous response started execution: err=%v effects=%d", err, *effects)
			}
		})
	}
}

func TestDurableAndLocalRunsCountTheSameToolCalls(t *testing.T) {
	localBackend, localTask, _, localEffects := durableFixture(t, &checkpointStore{})
	localTask.Durable = nil
	local := Run(context.Background(), localBackend, localTask, func(Event) {})
	durableBackend, durableTask, _, durableEffects := durableFixture(t, &checkpointStore{})
	durable := Run(context.Background(), durableBackend, durableTask, func(Event) {})
	if local.Status != StatusOK || durable.Status != StatusOK || local.ToolCalls != 1 || durable.ToolCalls != local.ToolCalls || *localEffects != 1 || *durableEffects != 1 {
		t.Fatalf("tool call counts differ: local=%+v durable=%+v effects=%d,%d", local, durable, *localEffects, *durableEffects)
	}
}

func TestDurableAndLocalRunsCountUnknownGuardedAndDeniedToolCallsTheSame(t *testing.T) {
	turn := []string{messageStart("msg_mixed_tools")}
	for i, call := range []struct{ id, name, input string }{
		{"unknown-1", "missing_tool", `{}`},
		{"guarded-1", "run_command", `{"command":"git push"}`},
		{"denied-1", "write_file", `{"path":"denied.txt","content":"no"}`},
	} {
		for _, event := range toolTurn(call.id, call.name, call.input)[1:4] {
			turn = append(turn, strings.ReplaceAll(event, `"index":0`, fmt.Sprintf(`"index":%d`, i)))
		}
	}
	turn = append(turn, messageEnd("tool_use")...)
	for _, durable := range []bool{false, true} {
		name := "local"
		if durable {
			name = "durable"
		}
		t.Run(name, func(t *testing.T) {
			backend, task, fake, _ := durableFixture(t, &checkpointStore{})
			backend.Tools = NewAPIBackend().Tools
			fake.turns = [][]string{turn, textTurn("done")}
			if !durable {
				task.Durable = nil
			}
			task.Mode = ModeAsk
			approvals := 0
			task.Approve = func(ApprovalRequest) bool { approvals++; return false }
			result := Run(context.Background(), backend, task, func(Event) {})
			if result.Status != StatusOK || result.ToolCalls != 3 || fake.served != 2 || approvals != 1 {
				t.Fatalf("tool calls were not counted: result=%+v requests=%d approvals=%d", result, fake.served, approvals)
			}
			messages := fake.requests[1]["messages"].([]any)
			outcomes := messages[len(messages)-1].(map[string]any)["content"].([]any)
			if len(outcomes) != 3 {
				t.Fatalf("tool outcomes = %d, want 3", len(outcomes))
			}
			for i, want := range []string{"unknown tool", "hrn guard", "Permission denied"} {
				outcome := outcomes[i].(map[string]any)
				if outcome["is_error"] != true || !strings.Contains(fmt.Sprint(outcome["content"]), want) {
					t.Fatalf("outcome %d was not refused as expected: %v", i, outcome)
				}
			}
		})
	}
}

func TestDurableRestoredResponseUsesTheOriginalCostBudget(t *testing.T) {
	store := &checkpointStore{failBoundary: "model_response", commitThenFail: true}
	backend, task, fake, effects := durableFixture(t, store)
	task.MaxCostUSD = 0.0001
	first := Run(context.Background(), backend, task, func(Event) {})
	if !first.RecoveryRequired || store.latest == nil {
		t.Fatalf("response was not checkpointed: %+v", first)
	}
	task.Durable.Resume = store.latest
	store.failBoundary = ""
	second := Run(context.Background(), backend, task, func(Event) {})
	if second.Status != StatusBudget || second.RecoveryRequired || second.ToolCalls != 1 || second.InputTokens != 100 || second.OutputTokens != 20 || *effects != 1 || fake.served != 1 {
		t.Fatalf("restored response changed budget or replayed an operation: %+v effects=%d requests=%d", second, *effects, fake.served)
	}
}

func TestDurablePauseTurnPersistsTheNextTurnBeforeContinuing(t *testing.T) {
	store := &checkpointStore{failBoundary: "turn_complete", commitThenFail: true}
	backend, task, fake, _ := durableFixture(t, store)
	fake.turns = [][]string{append([]string{messageStart("msg_paused")}, messageEnd("pause_turn")...), textTurn("done")}
	first := Run(context.Background(), backend, task, func(Event) {})
	if !first.RecoveryRequired || store.latest == nil || store.latest.NextTurn != 2 || fake.served != 1 {
		t.Fatalf("pause was not checkpointed: %+v requests=%d", first, fake.served)
	}
	task.Durable.Resume = store.latest
	store.failBoundary = ""
	second := Run(context.Background(), backend, task, func(Event) {})
	if second.Status != StatusOK || second.Final != "done" || second.Turns != 2 || fake.served != 2 {
		t.Fatalf("paused turn was replayed: %+v requests=%d", second, fake.served)
	}
}

func TestDurableRejectsLocalMCPBeforeStartingAnOperation(t *testing.T) {
	store := &checkpointStore{}
	backend, task, fake, _ := durableFixture(t, store)
	task.MCP = []string{"unused.json"}
	result := Run(context.Background(), backend, task, func(Event) {})
	if result.Status != StatusError || !strings.Contains(result.Error, "durable MCP execution requires an isolated executor") || fake.served != 0 || len(store.records) != 0 {
		t.Fatalf("local MCP was accepted: %+v requests=%d checkpoints=%d", result, fake.served, len(store.records))
	}
}

func TestDurableUsesCheckpointsInsteadOfAConversationFile(t *testing.T) {
	store := &checkpointStore{}
	backend, task, _, _ := durableFixture(t, store)
	task.RunDir = t.TempDir()
	result := Run(context.Background(), backend, task, func(Event) {})
	if result.Status != StatusOK || store.latest == nil || !store.latest.Finished {
		t.Fatalf("run did not finish through checkpoints: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(task.RunDir, "messages.json")); !os.IsNotExist(err) {
		t.Fatalf("durable run wrote a conversation file: %v", err)
	}
}
