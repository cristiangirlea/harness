package agent

import (
	"encoding/json"
	"math"
	"testing"
)

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
