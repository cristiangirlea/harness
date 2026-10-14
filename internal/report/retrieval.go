package report

import (
	"fmt"
	"github.com/cristiangirlea/harness/internal/agent"
	"math"
)

type Overhead struct {
	ModelTokens *int64   `json:"model_tokens"`
	CostUSD     *float64 `json:"cost_usd"`
	Estimated   bool     `json:"estimated,omitempty"`
	DurationNS  int64    `json:"duration_ns"`
}

// TaskUsage contains all attempts, including failures. Nil overhead values mean
// unknown; callers must explicitly supply zero for a deterministic operation.
type TaskUsage struct {
	Attempts []agent.Result `json:"attempts"`
	Passed   bool           `json:"passed"`
	Online   Overhead       `json:"online"`
}
type WorkflowUsage struct {
	ValidationError         string   `json:"validation_error,omitempty"`
	PreparationTokensKnown  bool     `json:"preparation_tokens_known"`
	PreparationCostKnown    bool     `json:"preparation_cost_known"`
	Tasks                   int      `json:"tasks"`
	Successes               int      `json:"successes"`
	KnownTokenTasks         int      `json:"known_token_tasks"`
	KnownCostTasks          int      `json:"known_cost_tasks"`
	Estimated               bool     `json:"estimated"`
	KnownTokens             int64    `json:"known_tokens"`
	KnownCostUSD            float64  `json:"known_cost_usd"`
	TokensPerSuccess        *float64 `json:"known_tokens_per_success"`
	CostPerSuccess          *float64 `json:"known_cost_per_success"`
	Preparation             Overhead `json:"preparation"`
	PreparationReuse        int      `json:"preparation_reuse"`
	AmortizedPreparationUSD *float64 `json:"amortized_preparation_usd"`
}

func SummarizeWorkflow(tasks []TaskUsage, prep Overhead, reuse int) WorkflowUsage {
	if err := ValidateWorkflow(tasks, prep, reuse); err != nil {
		return WorkflowUsage{ValidationError: err.Error()}
	}
	out := WorkflowUsage{PreparationTokensKnown: prep.ModelTokens != nil, PreparationCostKnown: prep.CostUSD != nil, Tasks: len(tasks), Preparation: prep, PreparationReuse: reuse, Estimated: prep.Estimated}
	if prep.CostUSD != nil {
		out.KnownCostUSD += *prep.CostUSD
		if reuse > 0 {
			v := *prep.CostUSD / float64(reuse)
			out.AmortizedPreparationUSD = &v
		}
	}
	if prep.ModelTokens != nil {
		out.KnownTokens += *prep.ModelTokens
	}
	for _, t := range tasks {
		if t.Passed {
			out.Successes++
		}
		kt, kc := len(t.Attempts) > 0 && t.Online.ModelTokens != nil, len(t.Attempts) > 0 && t.Online.CostUSD != nil
		out.Estimated = out.Estimated || t.Online.Estimated
		if t.Online.ModelTokens != nil {
			out.KnownTokens += *t.Online.ModelTokens
		}
		if t.Online.CostUSD != nil {
			out.KnownCostUSD += *t.Online.CostUSD
		}
		for _, r := range t.Attempts {
			n, k, e := r.ModelTokens()
			kt = kt && k
			out.KnownTokens += n
			out.Estimated = out.Estimated || e
			c, k, e := r.KnownCost()
			kc = kc && k
			out.KnownCostUSD += c
			out.Estimated = out.Estimated || e
		}
		if kt {
			out.KnownTokenTasks++
		}
		if kc {
			out.KnownCostTasks++
		}
	}
	if out.Successes > 0 {
		n := float64(out.KnownTokens) / float64(out.Successes)
		c := out.KnownCostUSD / float64(out.Successes)
		out.TokensPerSuccess = &n
		out.CostPerSuccess = &c
	}
	return out
}

func ValidateWorkflow(tasks []TaskUsage, prep Overhead, reuse int) error {
	if reuse < 0 {
		return fmt.Errorf("negative preparation reuse")
	}
	var tokens int64
	var cost float64
	add := func(n int64, c float64) error {
		if n < 0 || n > math.MaxInt64-tokens || c < 0 || math.IsNaN(c) || math.IsInf(c, 0) || math.IsInf(cost+c, 0) {
			return fmt.Errorf("negative, nonfinite or overflowing accounting")
		}
		tokens += n
		cost += c
		return nil
	}
	overhead := func(o Overhead) error {
		if o.DurationNS < 0 {
			return fmt.Errorf("negative overhead duration")
		}
		var n int64
		var c float64
		if o.ModelTokens != nil {
			n = *o.ModelTokens
		}
		if o.CostUSD != nil {
			c = *o.CostUSD
		}
		return add(n, c)
	}
	if err := overhead(prep); err != nil {
		return err
	}
	for _, t := range tasks {
		if err := overhead(t.Online); err != nil {
			return err
		}
		for _, r := range t.Attempts {
			if err := r.ValidateUsage(); err != nil {
				return err
			}
			n, _, _ := r.ModelTokens()
			if err := add(n, r.CostUSD); err != nil {
				return err
			}
		}
	}
	return nil
}
