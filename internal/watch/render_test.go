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

func TestLongToolInputKeepsSummaryAndPrettyBody(t *testing.T) {
	command := "echo sk-test-long-input"
	input, err := json.Marshal(map[string]string{"command": command, "content": strings.Repeat("ordinary text\n", 150), "env": "fake-env-value-12345"})
	if err != nil {
		t.Fatal(err)
	}
	s := &server{redact: strings.NewReplacer("fake-env-value-12345", "[redacted]")}
	e := s.cleanEvent(agent.Event{Kind: agent.EventToolCall, Tool: "Bash", Input: input})
	if !json.Valid(e.Input) || !strings.Contains(string(e.Input), "[redacted]") {
		t.Fatal("cleanEvent must preserve JSON and redact environment values")
	}
	v, _ := eventOf(Line{Event: e}, time.Time{})
	if v.Summary != "echo ••••" {
		t.Errorf("summary = %q", v.Summary)
	}
	if !strings.HasPrefix(v.Body, "{\n  \"command\": ") || !strings.Contains(v.Body, "echo ••••") || strings.Contains(v.Body, "sk-test-long-input") || !strings.Contains(v.Body, "[redacted]") || v.More != "" {
		t.Error("body must be complete pretty JSON with masked secrets")
	}
}

func TestLongResultKeepsCountsAndPageCaps(t *testing.T) {
	text := strings.Repeat(strings.Repeat("x", 99)+"\n", 512)
	s := &server{}
	v, _ := eventOf(Line{Event: s.cleanEvent(agent.Event{Kind: agent.EventToolResult, Text: text})}, time.Time{})
	if !strings.HasSuffix(v.Summary, " · 512 lines · 50.0 KB") {
		t.Errorf("summary = %q", v.Summary)
	}
	if !strings.Contains(v.More, "not shown here") || len(v.Body) < resultHead+resultTail {
		t.Error("long result must use the page head and tail caps")
	}
}

func TestResultTokensSayWhenUsageIsMissing(t *testing.T) {
	for _, tc := range []struct {
		usage *agent.UsageMetadata
		want  bool
	}{{nil, false}, {&agent.UsageMetadata{Status: "missing"}, true}} {
		v := resultOf(agent.Result{Status: agent.StatusOK, InputTokens: 5, Usage: tc.usage})
		tokens := ""
		for _, f := range v.Facts {
			if f.Label == "Tokens" {
				tokens = f.Value
			}
		}
		if got := strings.Contains(tokens, "usage missing"); got != tc.want || !strings.Contains(tokens, "5 in") {
			t.Errorf("usage %+v: tokens = %q", tc.usage, tokens)
		}
	}
}

func TestBatchPageShowsATaskDeclaredPurpose(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	res := batch.Result{ID: "b1", Tasks: []batch.TaskResult{
		{ID: "declared", Purpose: "review", Status: batch.StatusPass, Branch: "hrn/declared"},
		{ID: "plain", Status: batch.StatusPass, Branch: "hrn/plain"},
	}}
	body := batchBodyOf(res, viewer{})
	purposeOf := func(tv taskView) string {
		for _, f := range tv.Facts {
			if f.Label == "Purpose" {
				return f.Value
			}
		}
		return ""
	}
	if got := purposeOf(body.Tasks[0]); got != "review" {
		t.Errorf("declared task purpose = %q, want review: %+v", got, body.Tasks[0].Facts)
	}
	if got := purposeOf(body.Tasks[1]); got != "" {
		t.Errorf("a task with no declared purpose shows %q", got)
	}
}

func TestRunPageDisplaysContinuationRoute(t *testing.T) {
	p := runPageOf(runlog.Record{ContinueRoute: "fresh_handover"}, nil, time.Time{}, viewer{})
	for _, f := range p.Facts {
		if f.Value == "fresh_handover" {
			return
		}
	}
	t.Fatalf("missing route: %+v", p.Facts)
}
