package agent

import (
	"context"
	"math"
	"strings"
	"testing"
)

func TestEstimateCost(t *testing.T) {
	const M = 1_000_000
	for _, tc := range []struct {
		name  string
		model string
		r     Result
		want  float64
	}{
		{"input and output", "claude-opus-5", Result{InputTokens: M, OutputTokens: M}, 5 + 25},
		{"cache read at 0.1x input by default", "claude-opus-5", Result{CacheRead: M}, 0.50},
		{"fable 5.1 prices its own cache reads", "claude-fable-5-1", Result{CacheRead: M}, 0.25},
		{"so does opus 5.5", "claude-opus-5-5", Result{CacheRead: M}, 0.20},
		{"cache writes at 1.25x input", "claude-sonnet-5", Result{CacheWrite: M}, 2.50},
		{"a dated snapshot bills as its model", "claude-haiku-4-5-20251001", Result{InputTokens: M}, 1},
		{"so does a Vertex snapshot", "claude-haiku-4-5@20251001", Result{OutputTokens: M}, 5},
		{"an unknown model has no estimate", "claude-not-real", Result{InputTokens: M}, 0},
		{"a date only strips at the end", "claude-20251001-opus-5", Result{InputTokens: M}, 0},
	} {
		if got := estimateCost(tc.model, tc.r); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s: estimateCost(%s) = %v, want %v", tc.name, tc.model, got, tc.want)
		}
	}
}

// servedBy rewrites the model a turn's message_start reports.
func servedBy(turn []string, model string) []string {
	out := append([]string(nil), turn...)
	out[0] = strings.Replace(out[0], `"model":"claude-opus-5"`, `"model":"`+model+`"`, 1)
	return out
}

func TestAPIPricesEachTurnAtTheModelThatServedIt(t *testing.T) {
	dir := t.TempDir()
	// Asked for Opus 5; the second turn comes back from Sonnet 5, as a
	// server-side fallback can do, and the third from a model hrn cannot price.
	turns := [][]string{
		toolTurn("t1", "list_files", `{}`),
		servedBy(toolTurn("t2", "list_files", `{}`), "claude-sonnet-5"),
		servedBy(textTurn("done"), "claude-unlisted-x"),
	}
	res, _, _ := runFake(t, turns, Task{Prompt: "go", Dir: dir, Options: Options{Mode: ModeReadOnly, Model: "claude-opus-5"}})
	if res.Status != StatusOK {
		t.Fatalf("result = %+v", res)
	}
	// Every turn is 100 input and 20 output tokens.
	opus := (100*5.0 + 20*25.0) / 1e6
	sonnet := (100*2.0 + 20*10.0) / 1e6
	want := opus + sonnet + opus // the unpriced model falls back to the one asked for
	if math.Abs(res.CostUSD-want) > 1e-12 {
		t.Errorf("cost = %v, want %v: each turn at the price of the model that served it", res.CostUSD, want)
	}
}

func TestAPISaysWhenCostIsNotEstimated(t *testing.T) {
	var infos []string
	b := NewAPIBackend()
	b.APIKey, b.MaxRetries = "test", 0
	b.BaseURL = "http://127.0.0.1:1" // never reached: the notice comes first
	b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Mode: ModeAsk, Model: "claude-not-in-the-price-table"}},
		func(e Event) {
			if e.Kind == EventInfo {
				infos = append(infos, e.Text)
			}
		})
	if len(infos) != 1 || !strings.Contains(infos[0], "cost is not estimated") {
		t.Fatalf("info events = %v: a zero cost must not pass for free", infos)
	}
}
