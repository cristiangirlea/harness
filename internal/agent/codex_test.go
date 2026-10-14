package agent

import (
	"fmt"
	"math"
	"slices"
	"testing"
)

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
