package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
	"testing"
)

func TestHelperCodexCostStream(t *testing.T) {
	if os.Getenv("HRN_CODEX_COST_HELPER") != "1" {
		return
	}
	fmt.Println(`{"type":"thread.started","thread_id":"cost-session"}`)
	for range 2 {
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":500000,"cached_input_tokens":200000,"output_tokens":50000}}`)
	}
	fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"done"}}`)
	os.Exit(0)
}

func TestCodexRunStreamCost(t *testing.T) {
	for _, model := range []string{"gpt-6.1-sol", "unknown", ""} {
		t.Run(model, func(t *testing.T) {
			t.Setenv("HRN_CODEX_COST_HELPER", "1")
			b := NewCodexBackend()
			b.name, b.candidates = os.Args[0], nil
			b.args = func(Task, string) []string { return []string{"-test.run=^TestHelperCodexCostStream$", "--"} }
			var events []Event
			res := b.Run(context.Background(), Task{Prompt: "work", Dir: t.TempDir(), Options: Options{Model: model}}, func(e Event) { events = append(events, e) })
			if res.Status != StatusOK || res.Final != "done" || res.Session != "cost-session" || res.Turns != 2 {
				t.Fatalf("stream result = %+v", res)
			}
			if res.InputTokens != 1000000 || res.CacheRead != 400000 || res.OutputTokens != 100000 || res.Usage == nil || res.Usage.Status != "reported" {
				t.Fatalf("usage result = %+v", res)
			}
			if !slices.ContainsFunc(events, func(e Event) bool { return e.Kind == EventText && e.Text == "done" }) ||
				!slices.ContainsFunc(events, func(e Event) bool { return e.Kind == EventSession && e.Text == "cost-session" }) {
				t.Fatalf("missing stream events: %+v", events)
			}
			want, status := 0.0, "missing"
			if model == "gpt-6.1-sol" {
				want, status = 2.24, "estimated"
			}
			if math.Abs(res.CostUSD-want) > 1e-12 || res.CostEstimate != (want > 0) || res.Usage.CostStatus != status {
				t.Fatalf("cost = %v, estimate = %v, status = %s; want %v, %s", res.CostUSD, res.CostEstimate, res.Usage.CostStatus, want, status)
			}
		})
	}
}

func TestCodexStreamClearsEstimateWhenUsageBecomesMissing(t *testing.T) {
	for _, line := range []string{
		`{"type":"turn.completed"}`,
		`{"type":"turn.completed","usage":{"input_tokens":1}}`,
		`{"type":"turn.completed","usage":{"input_tokens":-1,"cached_input_tokens":0,"output_tokens":0}}`,
		fmt.Sprintf(`{"type":"turn.completed","usage":{"input_tokens":%d,"cached_input_tokens":0,"output_tokens":0}}`, int64(math.MaxInt64)),
		`{"type":"error","message":"failed"}`,
		`{"type":"turn.failed","error":{"message":"failed"}}`,
	} {
		t.Run(line, func(t *testing.T) {
			res := Result{Model: "gpt-6.1-sol"}
			valid := []byte(`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":2}}`)
			parseCodexStream(valid, func(Event) {}, &res)
			if !res.CostEstimate || res.CostUSD <= 0 {
				t.Fatalf("first turn was not estimated: %+v", res)
			}
			parseCodexStream([]byte(line), func(Event) {}, &res)
			for range 2 {
				if res.CostUSD != 0 || res.CostEstimate || res.Usage.Status != "missing" || res.Usage.CostStatus != "missing" {
					t.Fatalf("missing usage retained cost: %+v, usage %+v", res, res.Usage)
				}
				parseCodexStream(valid, func(Event) {}, &res)
			}
		})
	}
}

func TestCodexListPriceEstimate(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  float64
	}{
		// Two turns total 1M input (400k cached) and 100k output.
		// Sol: 0.6*2 + 0.4*0.10 + 0.1*10 = $2.24.
		{"gpt-6.1-sol", 2.24},
		{"gpt-6.1-sol-20261015", 2.24},
		{"gpt-6.1-sol@20261015", 2.24},
		// Luna: 0.6*0.10 + 0.4*0.01 + 0.1*0.50 = $0.114.
		{"gpt-6-luna", 0.114},
		// Astra: 0.6*10 + 0.4*1 + 0.1*50 = $11.40.
		{"gpt-6-astra", 11.4},
		{"unknown", 0},
		{"", 0},
		{"gpt-6-sol", 0}, // Not listed on the fetched pricing page.
	} {
		t.Run(tc.model, func(t *testing.T) {
			b := NewCodexBackend()
			res := Result{Backend: "codex", Model: tc.model}
			for range 2 {
				b.parse([]byte(`{"type":"turn.completed","usage":{"input_tokens":500000,"cached_input_tokens":200000,"output_tokens":50000}}`), func(Event) {}, &res)
			}
			wantStatus := "missing"
			if tc.want > 0 {
				wantStatus = "estimated"
			}
			if math.Abs(res.CostUSD-tc.want) > 1e-12 || res.CostEstimate != (tc.want > 0) || res.Usage.CostStatus != wantStatus {
				t.Fatalf("result = %+v, usage = %+v; want cost %v, status %s", res, res.Usage, tc.want, wantStatus)
			}
		})
	}
}

func TestCodexEstimateRequiresReportedUsage(t *testing.T) {
	for _, line := range []string{
		`{"type":"thread.started","thread_id":"test"}`,
		`{"type":"turn.completed"}`,
		`{"type":"turn.failed","error":{"message":"failed"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":2,"output_tokens":1}}`,
	} {
		b := NewCodexBackend()
		res := Result{Backend: "codex", Model: "gpt-6.1-sol"}
		b.parse([]byte(line), func(Event) {}, &res)
		if res.CostUSD != 0 || res.CostEstimate || (res.Usage != nil && res.Usage.CostStatus != "missing") {
			t.Fatalf("invalid or missing usage was priced: %+v", res)
		}
	}
}

func TestCodexReportedZeroCostIsEstimated(t *testing.T) {
	b := NewCodexBackend()
	res := Result{Backend: "codex", Model: "gpt-6.1-sol"}
	b.parse([]byte(`{"type":"turn.completed","usage":{"input_tokens":0,"cached_input_tokens":0,"output_tokens":0}}`), func(Event) {}, &res)
	if cost, known, estimated := res.KnownCost(); cost != 0 || !known || !estimated || !res.CostEstimate {
		t.Fatalf("reported zero: %+v, known=%v estimated=%v", res, known, estimated)
	}
}

func TestCodexOldResultLoads(t *testing.T) {
	var res Result
	if err := json.Unmarshal([]byte(`{"backend":"codex","model":"gpt-6.1-sol","status":"ok","turns":1,"input_tokens":100,"output_tokens":10,"duration_ns":1}`), &res); err != nil {
		t.Fatal(err)
	}
	if res.Backend != "codex" || res.InputTokens != 100 || res.CostUSD != 0 || res.CostEstimate || res.Usage != nil {
		t.Fatalf("old result = %+v", res)
	}
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"cost_usd", "cost_is_estimate", "usage_metadata"} {
		if _, ok := fields[key]; ok {
			t.Errorf("empty field %s was not omitted", key)
		}
	}
}

func TestCodexUsageOverflowStaysMissingAfterAnotherTurn(t *testing.T) {
	for _, field := range []string{"input_tokens", "cached_input_tokens", "output_tokens"} {
		t.Run(field, func(t *testing.T) {
			var res Result
			// Without checked addition, these three turns wrap back to positive 1.
			for _, n := range []int64{math.MaxInt64, math.MaxInt64, 3} {
				counters := map[string]int64{field: n}
				line := fmt.Sprintf(`{"type":"turn.completed","usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d}}`,
					counters["input_tokens"], counters["cached_input_tokens"], counters["output_tokens"])
				if !parseCodexStream([]byte(line), func(Event) {}, &res) {
					t.Fatal("turn.completed was not parsed")
				}
			}
			parseCodexStream([]byte(`{"type":"turn.completed","usage":{"input_tokens":0,"cached_input_tokens":0,"output_tokens":0}}`), func(Event) {}, &res)
			if res.Usage == nil || res.Usage.Status != "missing" || res.Usage.CostStatus != "missing" || res.Usage.InputSemantics != "inclusive_cache" {
				t.Errorf("overflowed usage = %+v, want missing", res.Usage)
			}
			got := map[string]int64{"input_tokens": res.InputTokens, "cached_input_tokens": res.CacheRead, "output_tokens": res.OutputTokens}
			if got[field] != math.MaxInt64 {
				t.Errorf("%s = %d, want last representable total %d", field, got[field], int64(math.MaxInt64))
			}
			if _, known, _ := res.ModelTokens(); known {
				t.Error("overflowed usage was counted as known")
			}
			if res.Turns != 4 {
				t.Errorf("turns = %d, want 4", res.Turns)
			}
		})
	}
}

func TestCodexUsageSumsRemainReportedWithinInt64(t *testing.T) {
	for _, tc := range []struct {
		name  string
		turns [][3]int64 // input, cache read, output
		want  [3]int64
	}{
		{"normal sums", [][3]int64{{10, 4, 2}, {20, 6, 3}}, [3]int64{30, 10, 5}},
		{"input limit", [][3]int64{{math.MaxInt64 - 1, 0, 0}, {1, 0, 0}}, [3]int64{math.MaxInt64, 0, 0}},
		{"output limit", [][3]int64{{0, 0, math.MaxInt64 - 1}, {0, 0, 1}}, [3]int64{0, 0, math.MaxInt64}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var res Result
			for _, turn := range tc.turns {
				line := fmt.Sprintf(`{"type":"turn.completed","usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d}}`, turn[0], turn[1], turn[2])
				if !parseCodexStream([]byte(line), func(Event) {}, &res) {
					t.Fatal("turn.completed was not parsed")
				}
			}
			if got := [3]int64{res.InputTokens, res.CacheRead, res.OutputTokens}; got != tc.want {
				t.Errorf("usage = %v, want %v", got, tc.want)
			}
			if res.Usage == nil || res.Usage.Status != "reported" || res.Usage.CostStatus != "missing" || res.Usage.InputSemantics != "inclusive_cache" {
				t.Errorf("usage metadata = %+v, want reported", res.Usage)
			}
			if _, known, _ := res.ModelTokens(); !known {
				t.Error("representable usage was counted as unknown")
			}
			if res.Turns != len(tc.turns) {
				t.Errorf("turns = %d, want %d", res.Turns, len(tc.turns))
			}
		})
	}
}

func TestCodexPassesEffortBeforeForkAndPrompt(t *testing.T) {
	for _, fork := range []string{"", "session-123"} {
		for _, effort := range []string{"", "high"} {
			t.Run("fork="+fork+"/effort="+effort, func(t *testing.T) {
				args := NewCodexBackend().args(Task{Dir: "/workspace", ForkSession: fork, Options: Options{Mode: ModeAuto, Effort: effort}}, "")
				config := slices.Index(args, "-c")
				if effort == "" {
					if config != -1 {
						t.Fatalf("unset effort must keep the CLI default: %v", args)
					}
				} else if config < 1 || config+1 >= len(args) || args[config+1] != "model_reasoning_effort=high" {
					t.Fatalf("want -c model_reasoning_effort=high among exec options: %v", args)
				}
				if args[0] != "exec" || args[len(args)-1] != "-" {
					t.Fatalf("want exec with a final stdin prompt: %v", args)
				}
				if effort != "" && config+1 >= len(args)-1 {
					t.Errorf("effort must precede the final -: %v", args)
				}
				if fork != "" {
					forkIndex := slices.Index(args, "fork")
					if forkIndex < 0 || forkIndex+1 >= len(args) || args[forkIndex+1] != fork {
						t.Fatalf("want fork %s: %v", fork, args)
					}
					if effort != "" && config+1 >= forkIndex {
						t.Errorf("effort must precede fork: %v", args)
					}
				}
			})
		}
	}
}

func TestACodexTurnThatCannotBeAddedLeavesEveryTotalAsItWas(t *testing.T) {
	for _, tc := range []struct {
		name string
		bad  [3]int64 // input, cache read, output
	}{
		{"input overflows", [3]int64{math.MaxInt64, 5, 7}},
		{"output overflows", [3]int64{5, 7, math.MaxInt64}},
		{"a negative counter", [3]int64{3, -1, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var res Result
			turn := func(c [3]int64) {
				line := fmt.Sprintf(`{"type":"turn.completed","usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d}}`, c[0], c[1], c[2])
				if !parseCodexStream([]byte(line), func(Event) {}, &res) {
					t.Fatal("turn.completed was not parsed")
				}
			}
			turn([3]int64{10, 4, 2})
			turn(tc.bad)
			if got := [3]int64{res.InputTokens, res.CacheRead, res.OutputTokens}; got != [3]int64{10, 4, 2} {
				t.Errorf("totals after the rejected turn = %v, want [10 4 2]", got)
			}
			turn([3]int64{1, 1, 1})
			if res.Usage == nil || res.Usage.Status != "missing" {
				t.Errorf("usage after a later valid turn = %+v, want missing", res.Usage)
			}
		})
	}
}
