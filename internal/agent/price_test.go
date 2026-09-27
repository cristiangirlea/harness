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
