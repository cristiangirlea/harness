package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/retrieval"
)

// Set UPDATE_GOLDEN=1 when deliberately updating the JSON contract fixtures.
func TestGoldenJSON(t *testing.T) {
	now := time.Date(2024, 2, 3, 4, 5, 6, 7000000, time.UTC)
	pin := &retrieval.Record{Scope: retrieval.Scope{TenantID: "tenant", ProjectID: "project", Backend: "api", Model: "scope-model", Revision: "revision"}, Mode: "required", OriginalPrompt: "original", Packet: &retrieval.Packet{VersionID: "version", AuditID: "audit", Entries: []retrieval.Entry{{ID: "entry", Key: "key", Content: "content", Source: "source", Evidence: "evidence", Identifiers: []string{"identifier"}}}, Text: "packet", Bytes: 6, Digest: "digest", QueryDigest: "query-digest", Ranking: "ranking", DurationNS: 31, EmptyReason: "reason"}, Failure: "failure", Replayed: true, DurationNS: 32}
	calls := 7
	result := Result{ChangedFiles: []string{"changed.go"}, ChangedFilesTruncated: true, Commits: []string{"commit"}, EndHead: "head", Retrieval: pin, Usage: &UsageMetadata{InputSemantics: "inclusive_cache", Status: "reported", CostStatus: "estimated", Calls: &calls}, RecoveryRequired: true, Backend: "backend", Model: "result-model", ModelReported: true, Status: StatusError, Final: "final", Error: "error", Turns: 8, InputTokens: 9, OutputTokens: 10, CacheRead: 11, CacheWrite: 12, ContextTokens: 13, ToolCalls: 14, CostUSD: 1.25, CostEstimate: true, Duration: 15 * time.Second, Session: "session", LimitResets: now, CLIVersion: "cli-version"}
	conv := Conversation{Retrieval: pin, Model: "conversation-model", Messages: []Message{{Role: "assistant", Content: []block{{"type": "text", "text": "hello"}}}}}
	task := Task{Knowledge: func() (retrieval.Searcher, error) { return nil, nil }, ChildEnv: []string{"CHILD=value"}, OriginalPrompt: "original prompt", RetrievalState: pin, RetrievalRepair: true, Durable: &DurableExecution{}, Prompt: "prompt", Purpose: "purpose", Dir: "project-dir", AttemptID: "attempt", Options: Options{ShapeTools: true, Retrieval: "required", RetrievalTenant: "tenant", RetrievalProject: "project", RetrievalRevision: "revision", RetrievalVersion: "version", Model: "task-model", Mode: ModeAsk, MaxTurns: 16, Effort: "high", MCP: []string{"mcp.json"}, NoInstructions: true, NoContextEdits: true, Lean: Bool(true), Skills: Bool(true), AppendSystemPrompt: "system", NoSkills: true, CompactWindow: "200000", AllowDangerous: true, InheritEnv: true, Timeout: Duration(17 * time.Second), MaxCostUSD: 2.5, TaskBudget: 21000, NoSwitch: true, Issue: "issue", TakeOver: true}, RunDir: "run-dir", Resume: &conv, ForkSession: "fork", FixRound: true, Approve: func(ApprovalRequest) bool { return true }}
	event := Event{Time: now, Kind: EventToolCall, Text: "event text", Tool: "tool", Input: json.RawMessage(`{"arg":"value"}`), IsError: true, Usage: &Usage{Model: "usage-model", InputTokens: 18, OutputTokens: 19, CacheRead: 20, CacheWrite: 21, ContextTokens: 22}, Quota: &Quota{FiveHour: &QuotaWindow{UsedPercent: 23.5, ResetsAt: 24}, Week: &QuotaWindow{UsedPercent: 25.5, ResetsAt: 26}}, Partial: true, Streamed: true}
	checkpoint := APICheckpoint{Version: 1, Sequence: 27, Boundary: "boundary", TaskDigest: "task-digest", Started: now, Deadline: now.Add(time.Hour), NextTurn: 28, Request: json.RawMessage(`{"request":true}`), Betas: []string{"beta"}, Response: json.RawMessage(`{"response":true}`), InvalidInput: map[string]string{"invalid": "input"}, ToolResults: []map[string]any{{"result": "tool-result"}}, Pending: &PendingOperation{Kind: "kind", ID: "pending-id", Tool: "pending-tool", Input: json.RawMessage(`{"pending":true}`)}, Result: result, Finished: true}
	for name, value := range map[string]any{"task": task, "task_auto_window": Task{Options: Options{CompactWindow: "auto"}}, "task_zero": Task{}, "result": result, "result_zero": Result{}, "event": event, "event_zero": Event{}, "conversation": conv, "conversation_zero": Conversation{}, "checkpoint": checkpoint, "checkpoint_zero": APICheckpoint{}} {
		t.Run(name, func(t *testing.T) { goldenJSON(t, name, value) })
	}
}

func goldenJSON(t *testing.T, name string, value any) {
	t.Helper()
	got, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "golden", name+".json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("JSON differs from %s\ngot: %s\nwant: %s", path, got, want)
	}
}
