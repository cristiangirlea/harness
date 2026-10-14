package report

import (
	"github.com/cristiangirlea/harness/internal/agent"
	"testing"
)

func TestWorkflowUsageCountsFailedAttemptsAndOverhead(t *testing.T) {
	tokens := int64(20)
	cost := 2.0
	tasks := []TaskUsage{{Passed: true, Online: Overhead{ModelTokens: &tokens, CostUSD: &cost}, Attempts: []agent.Result{{Backend: "codex", InputTokens: 100, CacheRead: 80, OutputTokens: 10, CostUSD: 3}}}, {Passed: false, Online: Overhead{ModelTokens: &tokens, CostUSD: &cost}, Attempts: []agent.Result{{Backend: "api", InputTokens: 100, CacheRead: 80, OutputTokens: 10, CostUSD: 5}}}}
	r := SummarizeWorkflow(tasks, Overhead{ModelTokens: &tokens, CostUSD: &cost}, 10)
	if r.KnownTokens != 360 || r.KnownCostUSD != 14 || *r.TokensPerSuccess != 360 || *r.CostPerSuccess != 14 || *r.AmortizedPreparationUSD != 0.2 {
		t.Fatalf("%#v", r)
	}
	tasks[1].Attempts[0] = agent.Result{}
	r = SummarizeWorkflow(tasks, Overhead{}, 0)
	if r.KnownTokenTasks != 1 || r.KnownCostTasks != 1 {
		t.Fatal("coverage", r)
	}
}
