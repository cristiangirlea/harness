package kernel

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOldResultsRoundTripWithoutToolStats(t *testing.T) {
	var r Result
	if err := json.Unmarshal([]byte(`{"backend":"fake","status":"ok","turns":1,"tool_calls":2,"duration_ns":0}`), &r); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(r)
	if err != nil || r.ToolStats != nil || strings.Contains(string(data), "tool_stats") || r.ToolCalls != 2 {
		t.Fatalf("result = %s, error = %v", data, err)
	}
	var again Result
	if err := json.Unmarshal(data, &again); err != nil || again.ToolCalls != 2 || again.ToolStats != nil {
		t.Fatalf("round trip = %+v, error = %v", again, err)
	}
}

func TestResultsRejectNegativeToolStats(t *testing.T) {
	for _, s := range []ToolStat{{Calls: -1}, {Errors: -1}, {ResultBytes: -1}} {
		if err := (Result{ToolStats: map[string]ToolStat{"read_file": s}}).ValidToolStats(); err == nil {
			t.Fatalf("accepted %+v", s)
		}
	}
}

func TestInvalidToolStatsDoNotHideKnownTokensOrCost(t *testing.T) {
	r := Result{Backend: "api", InputTokens: 10, OutputTokens: 2, CostUSD: 0.5,
		Usage:     &UsageMetadata{InputSemantics: "exclusive_cache", Status: "reported", CostStatus: "reported"},
		ToolStats: map[string]ToolStat{"read_file": {Calls: -1}}}
	if err := r.ValidateUsage(); err != nil {
		t.Fatal(err)
	}
	if tokens, known, _ := r.ModelTokens(); !known || tokens != 12 {
		t.Fatalf("tokens = %d, known = %t", tokens, known)
	}
	if cost, known, _ := r.KnownCost(); !known || cost != 0.5 {
		t.Fatalf("cost = %v, known = %t", cost, known)
	}
	if r.UsageMissing() {
		t.Fatal("valid model usage marked missing")
	}
}

func TestToolStatsOmitZeroCounts(t *testing.T) {
	data, err := json.Marshal(ToolStat{Calls: 1})
	if err != nil || string(data) != `{"calls":1}` {
		t.Fatalf("stats = %s, error = %v", data, err)
	}
}
