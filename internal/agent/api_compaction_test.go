package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/runlog"
)

func compactionToolTurn(id string, tokens int) []string {
	turn := toolTurn(id, "list_files", `{}`)
	turn[0] = strings.Replace(turn[0], `"input_tokens":100`, fmt.Sprintf(`"input_tokens":%d`, tokens), 1)
	return turn
}

func compactionTask(t *testing.T) Task {
	t.Helper()
	return Task{Prompt: "finish the task", Dir: t.TempDir(), Options: Options{CompactWindow: "20480", NoInstructions: true}}
}

func TestAPICompactionLeavesRequestsIdenticalWhenUnsetOrBelowThreshold(t *testing.T) {
	task := compactionTask(t)
	var baseline [][]byte
	for _, window := range []CompactWindow{"", "20480", "auto"} {
		task.CompactWindow = window
		res, events, fake := runFake(t, [][]string{compactionToolTurn("one", 20479), textTurn("done")}, task)
		if res.Status != StatusOK || len(fake.requests) != 2 {
			t.Fatalf("window=%q result=%+v", window, res)
		}
		if baseline == nil {
			baseline = fake.bodies
		} else if !reflect.DeepEqual(fake.bodies, baseline) {
			t.Fatalf("window=%q changed request bytes", window)
		}
		for _, event := range events {
			if event.Kind == EventCompaction {
				t.Fatal("compacted below the threshold")
			}
		}
	}
	task.CompactWindow = ""
	res, _, fake := runFake(t, [][]string{compactionToolTurn("one", 100000), textTurn("done")}, task)
	if res.Status != StatusOK || !reflect.DeepEqual(fake.bodies, baseline) {
		t.Fatal("unset window changed requests above the threshold")
	}
}

func TestAPICompactionSummarizesCompletedToolTurnsAndPreservesFullHistory(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	task := compactionTask(t)
	task.RunDir = t.TempDir()
	task.Effort, task.TaskBudget = "high", 30000
	res, events, fake := runFake(t, [][]string{
		compactionToolTurn("one", 20480), textTurn("summary one"),
		compactionToolTurn("two", 100), textTurn("done"),
	}, task)
	if res.Status != StatusOK || len(fake.requests) != 4 {
		t.Fatalf("result=%+v requests=%d", res, len(fake.requests))
	}
	summaryReq := fake.requests[1]
	for _, field := range []string{"thinking", "output_config", "cache_control"} {
		if _, present := fake.requests[0][field]; !present {
			t.Errorf("main request lost %s", field)
		}
		if _, present := summaryReq[field]; present {
			t.Errorf("summary request carries %s", field)
		}
	}
	if !reflect.DeepEqual(summaryReq["tools"], fake.requests[0]["tools"]) ||
		!reflect.DeepEqual(summaryReq["system"], fake.requests[0]["system"]) ||
		summaryReq["model"] != fake.requests[0]["model"] || summaryReq["max_tokens"] != float64(4096) ||
		!reflect.DeepEqual(summaryReq["tool_choice"], map[string]any{"type": "none"}) {
		t.Fatalf("summary request settings: %v", summaryReq)
	}
	msgs := capturedMessages(t, summaryReq)
	if len(msgs) != 3 || msgs[2].Content[0]["type"] != "tool_result" || len(msgs[2].Content) != 2 || msgs[2].Role != "user" {
		t.Fatalf("summary did not follow completed tool results: %v", msgs)
	}
	for _, phrase := range []string{"task", "success", "files", "commands", "failed", "next steps"} {
		if !strings.Contains(strings.ToLower(str(msgs[2].Content[1]["text"])), phrase) {
			t.Errorf("continuation prompt omits %q", phrase)
		}
	}
	next := capturedMessages(t, fake.requests[2])
	if len(next) != 1 || next[0].Role != "user" || !strings.Contains(str(next[0].Content[0]["text"]), task.Prompt) || !strings.Contains(str(next[0].Content[0]["text"]), "summary one") {
		t.Fatalf("compacted next request: %v", next)
	}
	last := capturedMessages(t, fake.requests[3])
	if len(last) != 3 || !reflect.DeepEqual(last[:1], next) || last[1].Content[0]["id"] != "two" || last[2].Content[0]["tool_use_id"] != "two" {
		t.Fatalf("turns after the cut point: %v", last)
	}
	data, err := os.ReadFile(filepath.Join(task.RunDir, "messages.json"))
	if err != nil {
		t.Fatal(err)
	}
	var conv Conversation
	if err := json.Unmarshal(data, &conv); err != nil || len(conv.Messages) != 6 || bytes.Contains(data, []byte("summary one")) {
		t.Fatalf("stored history changed: %s (%v)", data, err)
	}
	if res.InputTokens != 20780 || res.OutputTokens != 80 || res.Turns != 3 || math.Abs(res.CostUSD-estimateCost(DefaultAPIModel, res)) > 1e-12 {
		t.Fatalf("summary usage/cost missing: %+v", res)
	}
	count := 0
	for _, event := range events {
		if event.Kind == EventCompaction {
			count++
			if event.Text != "compact_window" || event.Usage == nil || *event.Usage != (Usage{ContextTokens: 20480}) {
				t.Fatalf("compaction event: %+v", event)
			}
		}
	}
	if count != 1 {
		t.Fatalf("compaction events=%d", count)
	}
	turns := runlog.Turns(events)
	var totals Usage
	for _, turn := range turns {
		totals.InputTokens += turn.InputTokens
		totals.OutputTokens += turn.OutputTokens
		totals.CacheRead += turn.CacheRead
		totals.CacheWrite += turn.CacheWrite
	}
	if len(turns) != 4 || totals.InputTokens != res.InputTokens || totals.OutputTokens != res.OutputTokens || totals.CacheRead != res.CacheRead || totals.CacheWrite != res.CacheWrite || turns[2].Trigger != "compact_window" || turns[2].CompactedFrom != 20480 {
		t.Fatalf("turns omit summary usage or change its trigger: %+v totals=%+v result=%+v", turns, totals, res)
	}
	for _, event := range events {
		if event.Kind == EventInfo && event.Text == "compaction summary: summary one" {
			return
		}
	}
	t.Fatal("summary text missing from info events")
}

func TestAPICompactionFailureContinuesWithFullHistoryAndAnErrorEvent(t *testing.T) {
	failure := []string{`{"type":"error","error":{"type":"invalid_request_error","message":"summary refused"}}`}
	res, events, fake := runFake(t, [][]string{compactionToolTurn("one", 20480), failure, textTurn("done")}, compactionTask(t))
	if res.Status != StatusOK || len(fake.requests) != 3 {
		t.Fatalf("result=%+v requests=%d", res, len(fake.requests))
	}
	if got := capturedMessages(t, fake.requests[2]); len(got) != 3 || got[1].Content[0]["id"] != "one" {
		t.Fatalf("failed compaction changed history: %v", got)
	}
	found := false
	for _, event := range events {
		if event.Kind == EventCompaction {
			t.Fatal("failed summary emitted a compaction")
		}
		found = found || event.Kind == EventError && strings.Contains(event.Text, "summary refused")
	}
	if !found {
		t.Fatal("summary failure omitted its error event")
	}
}

func TestAPICompactionRetainsUsageFromAFailedSummaryStream(t *testing.T) {
	failed := textTurn("partial summary")
	failed = append(failed[:len(failed)-2], `{"type":"error","error":{"type":"invalid_request_error","message":"summary interrupted"}}`)
	res, events, fake := runFake(t, [][]string{compactionToolTurn("one", 20480), failed, textTurn("done")}, compactionTask(t))
	if res.Status != StatusOK || len(fake.requests) != 3 || res.InputTokens != 20680 || res.OutputTokens != 41 {
		t.Fatalf("partial summary usage lost: %+v", res)
	}
	for _, event := range events {
		if event.Kind == EventCompaction {
			t.Fatal("partial summary replaced history")
		}
		if event.Kind == EventUsage && event.Usage.OutputTokens == 1 && event.Usage.ContextTokens != 100 {
			t.Fatalf("failed summary's usage context changed: %+v", event)
		}
	}
}

func TestAPICompactionRejectsSummariesThatCannotContinueTheTask(t *testing.T) {
	for _, stop := range []string{"empty", "max_tokens", "refusal", "tool_use"} {
		t.Run(stop, func(t *testing.T) {
			summary := textTurn("summary")
			if stop == "empty" {
				summary = textTurn(" ")
			} else {
				summary[len(summary)-2] = messageEnd(stop)[0]
			}
			res, events, fake := runFake(t, [][]string{compactionToolTurn("one", 20480), summary, textTurn("done")}, compactionTask(t))
			if res.Status != StatusOK || len(fake.requests) != 3 || len(capturedMessages(t, fake.requests[2])) != 3 {
				t.Fatalf("unusable summary changed history: %+v", res)
			}
			found := false
			for _, event := range events {
				if event.Kind == EventCompaction {
					t.Fatal("unusable summary emitted a compaction")
				}
				found = found || event.Kind == EventError && strings.Contains(event.Text, "compaction")
			}
			if !found {
				t.Fatal("unusable summary was not reported")
			}
		})
	}
}

func TestAPICompactionSummarizesTheCurrentOutgoingViewTwice(t *testing.T) {
	res, events, fake := runFake(t, [][]string{
		compactionToolTurn("one", 20480), textTurn("first summary"),
		compactionToolTurn("two", 20480), textTurn("second summary"), textTurn("done"),
	}, compactionTask(t))
	if res.Status != StatusOK || len(fake.requests) != 5 {
		t.Fatalf("result=%+v requests=%d", res, len(fake.requests))
	}
	second := capturedMessages(t, fake.requests[3])
	if len(second) != 3 || !strings.Contains(str(second[0].Content[0]["text"]), "first summary") || second[1].Content[0]["id"] != "two" {
		t.Fatalf("second summary read the wrong view: %v", second)
	}
	final := capturedMessages(t, fake.requests[4])
	if len(final) != 1 || !strings.Contains(str(final[0].Content[0]["text"]), "second summary") || strings.Contains(str(final[0].Content[0]["text"]), "first summary") {
		t.Fatalf("second compaction retained old turns: %v", final)
	}
	count := 0
	for _, event := range events {
		if event.Kind == EventCompaction {
			count++
		}
	}
	if count != 2 || res.InputTokens != 41260 || res.OutputTokens != 100 {
		t.Fatalf("compactions=%d result=%+v", count, res)
	}
}

func TestAPICompactionAppliesMaskingToTheRemainingOutgoingTurns(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	task := compactionTask(t)
	task.MaskToolResults, task.RunDir = 1, t.TempDir()
	if err := os.WriteFile(filepath.Join(task.Dir, "long.txt"), []byte(strings.Repeat("x", 300)), 0600); err != nil {
		t.Fatal(err)
	}
	res, _, fake := runFake(t, [][]string{
		compactionToolTurn("one", 20480), textTurn("summary"),
		toolTurn("two", "read_file", `{"path":"long.txt"}`),
		toolTurn("three", "read_file", `{"path":"long.txt"}`), textTurn("done"),
	}, task)
	if res.Status != StatusOK || len(fake.requests) != 5 {
		t.Fatalf("result=%+v requests=%d", res, len(fake.requests))
	}
	msgs := capturedMessages(t, fake.requests[4])
	if len(msgs) != 5 || !strings.HasPrefix(str(msgs[2].Content[0]["content"]), "[masked by hrn:") || strings.HasPrefix(str(msgs[4].Content[0]["content"]), "[masked by hrn:") {
		t.Fatalf("masking after compaction: %v", msgs)
	}
	data, err := os.ReadFile(filepath.Join(task.RunDir, "messages.json"))
	if err != nil || bytes.Contains(data, []byte("[masked by hrn:")) || !bytes.Contains(data, []byte(strings.Repeat("x", 300))) {
		t.Fatalf("masking changed stored history: %v", err)
	}
}

func TestAPIDurableResumeRebuildsTheSameCompactedRequest(t *testing.T) {
	store := &checkpointStore{}
	b, task, fake, _ := durableFixture(t, store)
	task.CompactWindow, task.NoInstructions = "20480", true
	first := toolTurn("one", "effect", `{}`)
	first[0] = strings.Replace(first[0], `"input_tokens":100`, `"input_tokens":20480`, 1)
	fake.turns = [][]string{first, textTurn("durable summary"), textTurn("done")}
	if res := b.Run(t.Context(), task, func(Event) {}); res.Status != StatusOK || len(fake.requests) != 3 {
		t.Fatalf("uninterrupted result=%+v requests=%d", res, len(fake.requests))
	}
	for i, cp := range store.records {
		if cp.Boundary != "compaction_complete" || cp.NextTurn != 2 {
			continue
		}
		var record map[string]any
		data, _ := json.Marshal(cp)
		_ = json.Unmarshal(data, &record)
		if record["compaction_k"] != float64(3) || record["compaction_summary"] != "durable summary" || cp.Pending != nil {
			t.Fatalf("checkpoint omitted compaction state: %s", data)
		}
		recovered := &checkpointStore{records: append([]APICheckpoint(nil), store.records[:i+1]...)}
		task.Durable = &DurableExecution{Resume: &cp, Save: recovered.save}
		fake.turns = append(fake.turns, textTurn("done"))
		res := b.Run(t.Context(), task, func(Event) {})
		if res.Status != StatusOK || len(fake.requests) != 4 || !bytes.Equal(fake.bodies[2], fake.bodies[3]) {
			t.Fatalf("resume changed request: %+v", res)
		}
		if res.InputTokens != 20680 || res.OutputTokens != 60 {
			t.Fatalf("resume lost summary usage: %+v", res)
		}
		return
	}
	t.Fatal("missing checkpoint after compaction")
}

func TestAPICompactionDoesNotSpendPastTheRunBudget(t *testing.T) {
	for _, durable := range []bool{false, true} {
		t.Run(fmt.Sprint(durable), func(t *testing.T) {
			b, task, fake, _ := durableFixture(t, &checkpointStore{})
			task.CompactWindow, task.MaxCostUSD = "20480", 0.1
			if !durable {
				task.Durable = nil
			}
			fake.turns = [][]string{compactionToolTurn("one", 20480), textTurn("summary")}
			res := b.Run(t.Context(), task, func(Event) {})
			if res.Status != StatusBudget || len(fake.requests) != 1 {
				t.Fatalf("summary spent after budget reached: %+v requests=%d", res, len(fake.requests))
			}
		})
	}
}

func TestAPIDurableCompactionPersistsIntentAndOutcomeBeforeContinuing(t *testing.T) {
	for _, boundary := range []string{"compaction_intent", "compaction_complete"} {
		for _, committed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/committed=%v", boundary, committed), func(t *testing.T) {
				store := &checkpointStore{failBoundary: boundary, commitThenFail: committed}
				b, task, fake, _ := durableFixture(t, store)
				task.CompactWindow = "20480"
				fake.turns = [][]string{compactionToolTurn("one", 20480), textTurn("summary"), textTurn("done")}
				res := b.Run(t.Context(), task, func(Event) {})
				requests := 1
				if boundary == "compaction_complete" {
					requests = 2
				}
				if res.Status != StatusError || !res.RecoveryRequired || len(fake.requests) != requests {
					t.Fatalf("crossed failed summary persistence: %+v requests=%d", res, len(fake.requests))
				}
				store.failBoundary = ""
				task.Durable.Resume = store.latest
				res = b.Run(t.Context(), task, func(Event) {})
				safe := boundary == "compaction_intent" && !committed || boundary == "compaction_complete" && committed
				if safe {
					if res.Status != StatusOK || len(fake.requests) != 3 {
						t.Fatalf("confirmed summary state did not resume: %+v requests=%d", res, len(fake.requests))
					}
				} else if !res.RecoveryRequired || len(fake.requests) != requests {
					t.Fatalf("ambiguous summary was repeated: %+v requests=%d", res, len(fake.requests))
				}
			})
		}
	}
}

func TestAPIDurableRecoveryDoesNotCompactAnAlreadyExhaustedBudget(t *testing.T) {
	store := &checkpointStore{failBoundary: "model_response", commitThenFail: true}
	b, task, fake, _ := durableFixture(t, store)
	task.CompactWindow, task.MaxCostUSD = "20480", 0.1
	fake.turns = [][]string{compactionToolTurn("one", 20480), textTurn("summary")}
	res := b.Run(t.Context(), task, func(Event) {})
	if !res.RecoveryRequired || len(fake.requests) != 1 {
		t.Fatalf("missing interrupted response: %+v", res)
	}
	store.failBoundary = ""
	task.Durable.Resume = store.latest
	res = b.Run(t.Context(), task, func(Event) {})
	if res.Status != StatusBudget || len(fake.requests) != 1 {
		t.Fatalf("summary spent after recovering an exhausted budget: %+v requests=%d", res, len(fake.requests))
	}
}

func TestAPIDurableFailedSummaryContinuesAndDoesNotRepeatOnRecovery(t *testing.T) {
	store := &checkpointStore{failBoundary: "compaction_complete", commitThenFail: true}
	b, task, fake, _ := durableFixture(t, store)
	task.CompactWindow = "20480"
	failed := textTurn("partial summary")
	failed = append(failed[:len(failed)-2], `{"type":"error","error":{"type":"invalid_request_error","message":"interrupted summary"}}`)
	fake.turns = [][]string{compactionToolTurn("one", 20480), failed, textTurn("done")}
	res := b.Run(t.Context(), task, func(Event) {})
	if !res.RecoveryRequired || len(fake.requests) != 2 || store.latest.Pending != nil || store.latest.Result.InputTokens != 20580 {
		t.Fatalf("failed summary outcome was not saved: %+v", res)
	}
	store.failBoundary = ""
	task.Durable.Resume = store.latest
	res = b.Run(t.Context(), task, func(Event) {})
	if res.Status != StatusOK || len(fake.requests) != 3 || len(capturedMessages(t, fake.requests[2])) != 3 || res.InputTokens != 20680 || res.OutputTokens != 41 {
		t.Fatalf("failed summary repeated or lost usage: %+v", res)
	}
}

func TestAPICompactionValidatesItsWindowBeforeMakingRequests(t *testing.T) {
	for _, window := range []CompactWindow{"16000", "20479", "20480", "32000", "1000000"} {
		t.Run(string(window), func(t *testing.T) {
			task := compactionTask(t)
			task.CompactWindow = window
			res, _, fake := runFake(t, [][]string{textTurn("done")}, task)
			bad := window == "16000" || window == "20479"
			if bad && (res.Status != StatusError || len(fake.requests) != 0 || !strings.Contains(res.Error, "20480")) || !bad && res.Status != StatusOK {
				t.Fatalf("window=%s result=%+v requests=%d", window, res, len(fake.requests))
			}
		})
	}
}

func TestClaudeRefusesCompactWindowsBelowItsCLIRangeBeforeStarting(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, window := range []CompactWindow{"20480", "50000", "99999"} {
		backend := NewClaudeBackend()
		backend.name = "claude"
		backend.candidates = nil
		res := backend.Run(t.Context(), Task{Options: Options{CompactWindow: window}}, func(Event) {})
		if res.Status != StatusError || !strings.Contains(res.Error, "compact_window") || !strings.Contains(res.Error, "100000") {
			t.Fatalf("window=%s result=%+v", window, res)
		}
	}
}

func TestAPICompactionWaitsUntilEveryToolOutcomeIsConfirmed(t *testing.T) {
	store := &checkpointStore{failBoundary: "tool_outcome", commitThenFail: true}
	b, task, fake, _ := durableFixture(t, store)
	task.CompactWindow = "20480"
	first := toolTurn("one", "effect", `{}`)
	first[0] = strings.Replace(first[0], `"input_tokens":100`, `"input_tokens":20480`, 1)
	fake.turns = [][]string{first, textTurn("summary"), textTurn("done")}
	res := b.Run(t.Context(), task, func(Event) {})
	if res.Status != StatusError || len(fake.requests) != 1 {
		t.Fatalf("summary crossed failed tool persistence: %+v", res)
	}
	store.failBoundary = ""
	task.Durable.Resume = store.latest
	res = b.Run(t.Context(), task, func(Event) {})
	if res.Status != StatusOK || len(fake.requests) != 3 {
		t.Fatalf("summary missing after confirmed recovery: %+v", res)
	}
}

func TestAPICompactionTriggersOnCachedContextAndCountsSummaryCacheUsage(t *testing.T) {
	first := compactionToolTurn("one", 100)
	first[0] = strings.Replace(first[0], `"input_tokens":100`, `"input_tokens":100,"cache_read_input_tokens":20000,"cache_creation_input_tokens":380`, 1)
	summary := textTurn("summary")
	summary[0] = strings.Replace(summary[0], `"input_tokens":100`, `"input_tokens":100,"cache_read_input_tokens":200,"cache_creation_input_tokens":300`, 1)
	res, events, fake := runFake(t, [][]string{first, summary, textTurn("done")}, compactionTask(t))
	if res.Status != StatusOK || len(fake.requests) != 3 || res.InputTokens != 300 || res.CacheRead != 20200 || res.CacheWrite != 680 || math.Abs(res.CostUSD-estimateCost(DefaultAPIModel, res)) > 1e-12 {
		t.Fatalf("cached summary accounting: %+v", res)
	}
	turns := runlog.Turns(events)
	var totals Usage
	for _, turn := range turns {
		totals.InputTokens += turn.InputTokens
		totals.OutputTokens += turn.OutputTokens
		totals.CacheRead += turn.CacheRead
		totals.CacheWrite += turn.CacheWrite
	}
	if len(turns) != 3 || totals.InputTokens != res.InputTokens || totals.OutputTokens != res.OutputTokens || totals.CacheRead != res.CacheRead || totals.CacheWrite != res.CacheWrite || turns[2].Trigger != "compact_window" {
		t.Fatalf("per-call totals omit cached summary usage: %+v result=%+v", turns, res)
	}
	for _, event := range events {
		if event.Kind == EventUsage && event.Usage.ContextTokens == 600 && event.Usage.CacheRead == 200 && event.Usage.CacheWrite == 300 {
			return
		}
	}
	t.Fatal("missing compaction with cached usage")
}

func TestAPIContinuationStartsFromFullHistoryAfterCompaction(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	task := compactionTask(t)
	task.RunDir = t.TempDir()
	res, _, _ := runFake(t, [][]string{compactionToolTurn("one", 20480), textTurn("old summary"), textTurn("done")}, task)
	if res.Status != StatusOK {
		t.Fatal(res.Error)
	}
	data, err := os.ReadFile(filepath.Join(task.RunDir, "messages.json"))
	if err != nil {
		t.Fatal(err)
	}
	var conv Conversation
	if err := json.Unmarshal(data, &conv); err != nil {
		t.Fatal(err)
	}
	task.Resume, task.Prompt = &conv, "continue the task"
	res, _, fake := runFake(t, [][]string{compactionToolTurn("two", 20480), textTurn("new summary"), textTurn("done again")}, task)
	if res.Status != StatusOK || len(fake.requests) != 3 {
		t.Fatalf("continuation: %+v", res)
	}
	start := capturedMessages(t, fake.requests[0])
	if len(start) != 5 || !reflect.DeepEqual(start[:4], conv.Messages) || start[4].Content[0]["text"] != task.Prompt {
		t.Fatalf("continuation omitted full history: %v", start)
	}
	last := capturedMessages(t, fake.requests[2])
	if len(last) != 1 || !strings.Contains(str(last[0].Content[0]["text"]), "new summary") || !strings.Contains(str(last[0].Content[0]["text"]), task.Prompt) {
		t.Fatalf("continuation did not compact again: %v", last)
	}
}

func TestAPICompactionRetriesFailedSummariesOnlyAfterAnotherWindowOfGrowth(t *testing.T) {
	for _, recover := range []bool{false, true} {
		for _, growth := range []bool{false, true} {
			t.Run(fmt.Sprintf("recover=%v/growth=%v", recover, growth), func(t *testing.T) {
				store := &checkpointStore{}
				b, task, fake, _ := durableFixture(t, store)
				task.CompactWindow, task.MaxTurns = "20480", 6
				if recover {
					store.failBoundary, store.commitThenFail = "compaction_complete", true
				} else {
					task.Durable = nil
				}
				fake.turns = [][]string{compactionToolTurn("one", 20480), {`{"type":"error","error":{"type":"invalid_request_error","message":"summary failed"}}`}}
				contexts := []int{21000, 22000}
				if growth {
					contexts = []int{21000, 24575, 24576}
				}
				for i, tokens := range contexts {
					fake.turns = append(fake.turns, compactionToolTurn(fmt.Sprint(i), tokens))
				}
				wantSummaries := 1
				if growth {
					fake.turns = append(fake.turns, textTurn("retry summary"))
					wantSummaries++
				}
				fake.turns = append(fake.turns, textTurn("done"))
				res := b.Run(t.Context(), task, func(Event) {})
				if recover {
					data, _ := json.Marshal(store.latest)
					var cp map[string]any
					_ = json.Unmarshal(data, &cp)
					if !res.RecoveryRequired || cp["compaction_failed_context"] != float64(20480) {
						t.Fatalf("failed summary retry point was not saved: result=%+v checkpoint=%s", res, data)
					}
					store.failBoundary = ""
					task.Durable.Resume = store.latest
					res = b.Run(t.Context(), task, func(Event) {})
				}
				summaries := 0
				for _, req := range fake.requests {
					if req["tool_choice"] != nil {
						summaries++
					}
				}
				if res.Status != StatusOK || res.Final != "done" || summaries != wantSummaries {
					t.Fatalf("summary retries=%d want=%d result=%+v", summaries, wantSummaries, res)
				}
			})
		}
	}
}

func TestAPICompactionPreservesSignedThinkingInSummaryAndRemainingTurns(t *testing.T) {
	thinking := func(signature string) []byte {
		data, _ := json.Marshal(block{"type": "thinking", "thinking": "reasoning\nwith detail", "signature": signature, "opaque": "kept"})
		return data
	}
	signedTurn := func(id string, tokens int, signature string) []string {
		turn := compactionToolTurn(id, tokens)
		result := []string{turn[0], `{"type":"content_block_start","index":0,"content_block":` + string(thinking(signature)) + `}`, `{"type":"content_block_stop","index":0}`}
		for _, event := range turn[1:] {
			result = append(result, strings.ReplaceAll(event, `"index":0`, `"index":1`))
		}
		return result
	}
	res, _, fake := runFake(t, [][]string{
		signedTurn("one", 20480, "first\nsignature"), textTurn("summary"),
		signedTurn("two", 100, "second\\signature"), textTurn("done"),
	}, compactionTask(t))
	if res.Status != StatusOK || len(fake.requests) != 4 {
		t.Fatalf("result=%+v requests=%d", res, len(fake.requests))
	}
	for _, tc := range []struct {
		request   int
		signature string
	}{{1, "first\nsignature"}, {3, "second\\signature"}} {
		msgs := capturedMessages(t, fake.requests[tc.request])
		data, _ := json.Marshal(msgs[1].Content[0])
		if !bytes.Equal(data, thinking(tc.signature)) {
			t.Fatalf("signed thinking changed in request %d: %s", tc.request, data)
		}
	}
}

func TestAPICompactionDoesNotBuyASummaryOnTheLastAllowedTurn(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	task := compactionTask(t)
	task.MaxTurns = 1
	res, _, fake := runFake(t, [][]string{compactionToolTurn("one", 20480), textTurn("unused summary")}, task)
	if res.Status != StatusMaxTurns || len(fake.requests) != 1 {
		t.Fatalf("status=%s requests=%d; want max_turns after one request", res.Status, len(fake.requests))
	}
}
