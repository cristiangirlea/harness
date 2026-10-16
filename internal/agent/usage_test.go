package agent

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// usageOf keeps the usage and compaction events of a run, in order.
func usageOf(events []Event) []Event {
	var out []Event
	for _, e := range events {
		if e.Kind == EventUsage || e.Kind == EventCompaction {
			out = append(out, e)
		}
	}
	return out
}

// The fixture is written from Claude Code's stream-json format: each content
// block of one message is a line of its own carrying the message's usage, a
// sub-agent's lines name the tool call that started it, and an automatic
// compaction is a system line with subtype compact_boundary.
func TestClaudeRecordsOneUsageEventPerMainLoopMessage(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("testdata", "claude-usage.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HRN_USAGE_HELPER", fixture)
	claude := NewClaudeBackend()
	b := helperCLI(claude.parse)
	b.newParse = claude.newParse
	b.args = func(Task, string) []string { return []string{"-test.run=^TestHelperCLIUsage$", "--"} }
	var events []Event
	res := b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir()}, func(e Event) { events = append(events, e) })
	if res.Status != StatusOK {
		t.Fatalf("result = %+v", res)
	}
	got := usageOf(events)
	want := []Event{
		{Kind: EventUsage, Usage: &Usage{Model: "claude-opus-5", InputTokens: 3, OutputTokens: 8, CacheWrite: 12000, ContextTokens: 12003}},
		{Kind: EventUsage, Usage: &Usage{Model: "claude-opus-5", InputTokens: 5, OutputTokens: 40, CacheRead: 12000, CacheWrite: 30000, ContextTokens: 42005}},
		{Kind: EventCompaction, Text: "auto", Usage: &Usage{ContextTokens: 42045}},
		{Kind: EventUsage, Usage: &Usage{Model: "claude-opus-5", InputTokens: 4, OutputTokens: 6, CacheWrite: 9000, ContextTokens: 9004}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d usage and compaction events, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Kind != want[i].Kind || got[i].Text != want[i].Text || *got[i].Usage != *want[i].Usage || got[i].Time.IsZero() {
			t.Errorf("event %d = %+v %+v, want %+v %+v", i, got[i], got[i].Usage, want[i], want[i].Usage)
		}
	}
	if res.ContextTokens != 9004 {
		t.Errorf("context = %d, want the last main-loop message's 9004", res.ContextTokens)
	}
}

// A message's output count grows over its lines, so its usage event carries
// the largest, the last message's included when the stream ends on it.
func TestClaudeKeepsTheLargestOutputAcrossAMessagesLines(t *testing.T) {
	line := func(id string, output int) string {
		return `{"type":"assistant","parent_tool_use_id":null,"session_id":"s","message":{"id":"` + id +
			`","model":"m","content":[{"type":"text","text":"x"}],"usage":{"input_tokens":3,"output_tokens":` +
			strconv.Itoa(output) + `,"cache_read_input_tokens":0,"cache_creation_input_tokens":100}}}`
	}
	fixture := filepath.Join(t.TempDir(), "stream.jsonl")
	stream := line("msg_01", 8) + "\n" + line("msg_01", 30) + "\n" + line("msg_01", 20) + "\n" +
		`{"type":"user","parent_tool_use_id":null,"session_id":"s","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":"ok"}]}}` + "\n" +
		line("msg_02", 5) + "\n" + line("msg_02", 12) + "\n"
	if err := os.WriteFile(fixture, []byte(stream), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HRN_USAGE_HELPER", fixture)
	claude := NewClaudeBackend()
	b := helperCLI(claude.parse)
	b.newParse = claude.newParse
	b.args = func(Task, string) []string { return []string{"-test.run=^TestHelperCLIUsage$", "--"} }
	var events []Event
	b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir()}, func(e Event) { events = append(events, e) })
	got := usageOf(events)
	if len(got) != 2 || got[0].Usage.OutputTokens != 30 || got[1].Usage.OutputTokens != 12 {
		for _, e := range got {
			t.Logf("%+v", *e.Usage)
		}
		t.Fatalf("got %d usage events, want two with outputs 30 and 12", len(got))
	}
}

// A usage event is recorded in events.jsonl under its own kind, and a line
// without one still reads as before.
func TestUsageEventRoundTrips(t *testing.T) {
	e := Event{Kind: EventUsage, Usage: &Usage{Model: "m", InputTokens: 1, CacheRead: 2, ContextTokens: 3}}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["kind"]) != `"usage"` || string(fields["usage"]) != `{"model":"m","input_tokens":1,"cache_read_tokens":2,"context_tokens":3}` {
		t.Errorf("recorded as %s", data)
	}
	var old Event
	if err := json.Unmarshal([]byte(`{"t":"2026-10-01T10:00:00Z","kind":"text","text":"hi"}`), &old); err != nil || old.Usage != nil {
		t.Errorf("an old line read as %+v (%v)", old, err)
	}
}

// TestHelperCLIUsage is not a test: the test binary prints a recorded stream.
func TestHelperCLIUsage(t *testing.T) {
	path := os.Getenv("HRN_USAGE_HELPER")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		os.Exit(2)
	}
	os.Stdout.Write(data)
	os.Exit(0)
}

// The api loop records every request's usage as the response reported it.
func TestAPIRecordsEachRequestsUsage(t *testing.T) {
	first := toolTurn("list", "list_files", `{"pattern":"*"}`)
	last := textTurn("done")
	last[0] = `{"type":"message_start","message":{"id":"last","role":"assistant","model":"claude-opus-5","usage":{"input_tokens":5000,"cache_read_input_tokens":270000,"cache_creation_input_tokens":10000}}}`
	res, events, _ := runFake(t, [][]string{first, last}, Task{Prompt: "list", Dir: t.TempDir()})
	if res.Status != StatusOK {
		t.Fatalf("result = %+v", res)
	}
	got := usageOf(events)
	want := []Usage{
		{Model: "claude-opus-5", InputTokens: 100, OutputTokens: 20, ContextTokens: 100},
		{Model: "claude-opus-5", InputTokens: 5000, OutputTokens: 20, CacheRead: 270000, CacheWrite: 10000, ContextTokens: 285000},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d usage events, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Kind != EventUsage || *got[i].Usage != want[i] {
			t.Errorf("usage %d = %+v, want %+v", i, got[i].Usage, want[i])
		}
	}
}

// opencode reports each step's tokens; a step that reported none writes no
// event.
func TestOpencodeRecordsEachStepsUsage(t *testing.T) {
	var events []Event
	var res Result
	for _, l := range []string{
		`{"type":"step_finish","part":{"tokens":{"input":6000,"output":900,"cache":{"read":300000,"write":20000}}}}`,
		`{"type":"step_finish","part":{}}`,
		`{"type":"step_finish","part":{"tokens":{"input":5000,"output":700,"cache":{"read":270000,"write":10000}}}}`,
	} {
		parseOpencodeStream([]byte(l), func(e Event) { events = append(events, e) }, &res)
	}
	got := usageOf(events)
	want := []Usage{
		{InputTokens: 6000, OutputTokens: 900, CacheRead: 300000, CacheWrite: 20000, ContextTokens: 326000},
		{InputTokens: 5000, OutputTokens: 700, CacheRead: 270000, CacheWrite: 10000, ContextTokens: 285000},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d usage events, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if *got[i].Usage != want[i] || got[i].Time.IsZero() {
			t.Errorf("usage %d = %+v, want %+v", i, got[i].Usage, want[i])
		}
	}
}

func TestModelTokensCountCacheAccordingToBackend(t *testing.T) {
	for _, tc := range []struct {
		backend string
		want    int64
		known   bool
	}{{"codex", 110, true}, {"api", 190, true}, {"claude", 190, true}, {"gemini", 0, false}} {
		r := Result{Backend: tc.backend, InputTokens: 100, OutputTokens: 10, CacheRead: 80}
		n, known, estimated := r.ModelTokens()
		if n != tc.want || known != tc.known || estimated {
			t.Errorf("%s: tokens=%d known=%t estimated=%t", tc.backend, n, known, estimated)
		}
	}
}

func TestModelTokensPreserveMissingAndEstimatedUsage(t *testing.T) {
	var old Result
	if err := json.Unmarshal([]byte(`{"backend":"codex","input_tokens":100,"output_tokens":10,"cache_read_tokens":80}`), &old); err != nil {
		t.Fatal(err)
	}
	if n, known, _ := old.ModelTokens(); !known || n != 110 || old.Usage != nil {
		t.Fatalf("legacy usage: %+v, tokens=%d known=%t", old, n, known)
	}
	old.Usage = &UsageMetadata{Status: "missing", InputSemantics: "inclusive_cache"}
	if _, known, _ := old.ModelTokens(); known {
		t.Fatal("explicit missing usage was treated as known")
	}
	old.Usage.Status = "estimated"
	if n, known, estimated := old.ModelTokens(); n != 110 || !known || !estimated {
		t.Fatalf("estimated usage: tokens=%d known=%t estimated=%t", n, known, estimated)
	}
}

func TestKnownCostDistinguishesReportedZeroFromMissing(t *testing.T) {
	for _, tc := range []struct {
		status    string
		cost      float64
		known     bool
		estimated bool
	}{{"reported", 0, true, false}, {"estimated", 0.2, true, true}, {"missing", 0, false, false}} {
		r := Result{CostUSD: tc.cost, Usage: &UsageMetadata{CostStatus: tc.status}}
		cost, known, estimated := r.KnownCost()
		if cost != tc.cost || known != tc.known || estimated != tc.estimated {
			t.Errorf("%s: cost=%v known=%t estimated=%t", tc.status, cost, known, estimated)
		}
	}
	if _, known, _ := (Result{}).KnownCost(); known {
		t.Fatal("legacy absent cost was treated as reported zero")
	}
}

func TestModelTokensRejectInvalidCounters(t *testing.T) {
	for _, r := range []Result{
		{Backend: "codex", InputTokens: 5, CacheRead: 6},
		{Backend: "api", InputTokens: -1},
		{Backend: "api", InputTokens: math.MaxInt64, OutputTokens: 1},
	} {
		if _, known, _ := r.ModelTokens(); known {
			t.Errorf("invalid counters accepted: %+v", r)
		}
	}
}
