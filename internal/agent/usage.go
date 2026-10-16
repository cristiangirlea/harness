package agent

import (
	"fmt"
	"math"
)

// UsageMetadata, in Result.Usage, describes how to interpret the run's totals;
// the counts themselves are Result's InputTokens, OutputTokens, CacheRead,
// CacheWrite and CostUSD.
// Usage in Event.Usage holds per-call token counters instead.
// Empty metadata on historical records is inferred only for known adapters.
type UsageMetadata struct {
	InputSemantics string `json:"input_semantics"`       // inclusive_cache or exclusive_cache
	Status         string `json:"status"`                // reported, estimated, missing
	CostStatus     string `json:"cost_status,omitempty"` // reported, estimated, missing
	Calls          *int   `json:"calls,omitempty"`
}

// ModelTokens counts each model token once. Missing/unknown semantics are not zero.
func (r Result) ModelTokens() (int64, bool, bool) {
	if r.ValidateUsage() != nil {
		return 0, false, false
	}
	semantics, status := "", ""
	if r.Usage != nil {
		semantics, status = r.Usage.InputSemantics, r.Usage.Status
	} else if r.InputTokens+r.OutputTokens+r.CacheRead+r.CacheWrite > 0 {
		status = "reported"
		switch r.Backend {
		case "codex":
			semantics = "inclusive_cache"
		case "api", "claude", "agy":
			semantics = "exclusive_cache"
		}
	}
	if r.InputTokens < 0 || r.OutputTokens < 0 || r.CacheRead < 0 || r.CacheWrite < 0 {
		return 0, false, false
	}
	total := r.InputTokens + r.OutputTokens
	switch semantics {
	case "inclusive_cache":
		if r.CacheRead+r.CacheWrite > r.InputTokens {
			return 0, false, false
		}
	case "exclusive_cache":
		total += r.CacheRead + r.CacheWrite
	default:
		return 0, false, false
	}
	return total, status == "reported" || status == "estimated", status == "estimated"
}

// KnownCost distinguishes an explicitly reported zero from absent cost.
func (r Result) KnownCost() (float64, bool, bool) {
	if r.ValidateUsage() != nil {
		return 0, false, false
	}
	if r.Usage != nil {
		switch r.Usage.CostStatus {
		case "reported":
			return r.CostUSD, true, r.CostEstimate
		case "estimated":
			return r.CostUSD, true, true
		case "missing":
			return 0, false, false
		}
	}
	return r.CostUSD, r.CostUSD > 0, r.CostEstimate
}

func (r Result) ValidateUsage() error {
	var sum int64
	for _, v := range []int64{r.InputTokens, r.OutputTokens, r.CacheRead, r.CacheWrite} {
		if v < 0 || v > math.MaxInt64-sum {
			return fmt.Errorf("negative or overflowing usage")
		}
		sum += v
	}
	if math.IsNaN(r.CostUSD) || math.IsInf(r.CostUSD, 0) || r.CostUSD < 0 || r.Duration < 0 || r.Turns < 0 || r.ToolCalls < 0 || (r.Usage != nil && r.Usage.Calls != nil && *r.Usage.Calls < 0) {
		return fmt.Errorf("invalid usage or cost")
	}
	return nil
}
