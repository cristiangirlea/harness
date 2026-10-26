package agent

import (
	"fmt"
	"math"
)

// UsageMetadata describes run totals, inferred only for known adapters.
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
		case "codex", "gemini":
			semantics = "inclusive_cache"
		case "api", "claude", "agy", "opencode":
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

// UsageMissing marks counters unsafe to display or sum as known.
func (r Result) UsageMissing() bool {
	return r.ValidateUsage() != nil || (r.Usage != nil && r.Usage.Status == "missing")
}

func (r *Result) addUsage(semantics string, input, output, cacheRead, cacheWrite int64) bool {
	for _, c := range [][2]int64{{r.InputTokens, input}, {r.OutputTokens, output}, {r.CacheRead, cacheRead}, {r.CacheWrite, cacheWrite}} {
		if c[0] < 0 || c[1] < 0 || c[1] > math.MaxInt64-c[0] {
			if r.Usage == nil {
				r.Usage = &UsageMetadata{InputSemantics: semantics}
			}
			r.Usage.Status = "missing"
			return false
		}
	}
	r.InputTokens += input
	r.OutputTokens += output
	r.CacheRead += cacheRead
	r.CacheWrite += cacheWrite
	return true
}
