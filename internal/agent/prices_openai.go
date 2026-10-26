package agent

import "regexp"

// openAIPrices are USD per million tokens at standard short-context list rates.
var openAIPrices = map[string]price{
	"gpt-6.1-sol": {in: 2, cacheRead: 0.10, out: 10},
	"gpt-6-luna":  {in: 0.10, cacheRead: 0.01, out: 0.50},
	"gpt-6-astra": {in: 10, cacheRead: 1, out: 50},
}

var openAIDated = regexp.MustCompile(`-\d{4}-\d{2}-\d{2}$`)

func estimateCodexCost(res *Result) {
	res.CostUSD, res.CostEstimate = 0, false
	if res.Usage == nil {
		res.Usage = &UsageMetadata{InputSemantics: "inclusive_cache", Status: "missing"}
	}
	res.Usage.CostStatus = "missing"
	p, ok := openAIPrices[openAIDated.ReplaceAllString(datedSuffix.ReplaceAllString(res.Model, ""), "")]
	if !ok || res.Usage.Status != "reported" {
		return
	}
	if _, known, _ := res.ModelTokens(); !known {
		return
	}
	cacheRead := p.cacheRead
	if cacheRead == 0 {
		cacheRead = 0.1 * p.in
	}
	// Codex input includes cache reads; bill those tokens only at the cached rate.
	res.CostUSD = (float64(res.InputTokens-res.CacheRead)*p.in +
		float64(res.CacheRead)*cacheRead + float64(res.OutputTokens)*p.out) / 1e6
	res.CostEstimate = true
	res.Usage.CostStatus = "estimated"
}
