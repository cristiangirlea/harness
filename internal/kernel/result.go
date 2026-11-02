package kernel

import (
	"fmt"
	"math"
	"time"

	"github.com/cristiangirlea/harness/internal/retrieval"
)

// MaxLimitWindow bounds plausible subscription reset times.
const MaxLimitWindow = 8 * 24 * time.Hour

type Result struct {
	ChangedFiles          []string          `json:"changed_files,omitempty"`
	ChangedFilesTruncated bool              `json:"changed_files_truncated,omitempty"`
	Commits               []string          `json:"commits,omitempty"`
	EndHead               string            `json:"end_head,omitempty"`
	Retrieval             *retrieval.Record `json:"retrieval,omitempty"`
	// Usage describes the run-level token and cost totals.
	Usage *UsageMetadata `json:"usage_metadata,omitempty"`
	// RecoveryRequired requires reconciliation before replay.
	RecoveryRequired bool   `json:"recovery_required,omitempty"`
	Backend          string `json:"backend"`
	Model            string `json:"model,omitempty"`
	// ModelReported distinguishes actual and requested model names.
	ModelReported bool   `json:"model_reported,omitempty"`
	Status        Status `json:"status"`
	Final         string `json:"final,omitempty"`
	Error         string `json:"error,omitempty"`
	Turns         int    `json:"turns"`
	InputTokens   int64  `json:"input_tokens,omitempty"`
	OutputTokens  int64  `json:"output_tokens,omitempty"`
	CacheRead     int64  `json:"cache_read_tokens,omitempty"`
	CacheWrite    int64  `json:"cache_write_tokens,omitempty"`
	// ContextTokens includes caches for the last call; totals-only reports omit it.
	ContextTokens int64 `json:"context_tokens,omitempty"`
	// ToolCalls counts emitted calls or durable committed outcomes.
	ToolCalls    int           `json:"tool_calls,omitempty"`
	CostUSD      float64       `json:"cost_usd,omitempty"`
	CostEstimate bool          `json:"cost_is_estimate,omitempty"`
	Duration     time.Duration `json:"duration_ns"`
	// Session permits CLI access and continuation.
	Session string `json:"session,omitempty"`
	// LimitResets is absent when the CLI gives no time.
	LimitResets time.Time `json:"limit_resets,omitzero"`
	// CLIVersion is the version the CLI recorded for this run.
	CLIVersion string `json:"cli_version,omitempty"`
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
