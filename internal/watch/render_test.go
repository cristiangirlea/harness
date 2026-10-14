package watch

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestDisplayMasksBeforeExcerpts(t *testing.T) {
	secret := "Authorization: Bearer " + strings.Repeat("z", 400)
	check := func(where string, v any) {
		t.Helper()
		raw, _ := json.Marshal(v)
		if strings.Contains(string(raw), "zz") || !strings.Contains(string(raw), "••••") {
			t.Errorf("%s lacks masking: %s", where, raw)
		}
	}
	bad, _ := eventOf(Line{Err: errors.New(secret)}, time.Time{})
	check("unreadable", bad)
	for _, kind := range []agent.EventKind{agent.EventText, agent.EventThinking, agent.EventToolCall, agent.EventToolResult, agent.EventInfo, agent.EventCompaction, agent.EventError} {
		v, _ := eventOf(Line{Event: agent.Event{Kind: kind, Text: secret}}, time.Time{})
		check(string(kind), v)
	}
	v, _ := eventOf(Line{Event: agent.Event{Kind: agent.EventToolCall, Input: json.RawMessage(`{"password":"` + strings.Repeat("z", 400) + `"}`)}}, time.Time{})
	check("input", v)
	rec := runlog.Record{Task: agent.Task{Prompt: secret}, Result: agent.Result{Error: secret, Final: secret}, Done: true}
	check("page", runPageOf(rec, nil, time.Time{}, viewer{}))
	check("row", runRow(rec))
	check("result", resultOf(rec.Result))
	s := &server{}
	res := batch.Result{Tasks: []batch.TaskResult{{Prompt: secret, Error: secret, Attempts: []batch.Attempt{{Error: secret, Result: rec.Result, Gates: []batch.GateResult{{Command: secret, Output: secret}}}}}}}
	check("batch", batchBodyOf(s.cleanBatch(res), viewer{}))
	if res.Tasks[0].Prompt != secret || res.Tasks[0].Attempts[0].Gates[0].Command != secret {
		t.Fatal("batch record mutated")
	}
}
