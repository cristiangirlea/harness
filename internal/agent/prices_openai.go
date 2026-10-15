package agent

// openAIPrices are USD per million tokens at OpenAI's standard short-context
// list prices, read on 2026-10-15 from https://developers.openai.com/api/docs/pricing.
// These are comparison estimates, not bills: service tiers, long context and
// regional uplifts are not represented. gpt-6-sol was not listed on that page.
var openAIPrices = map[string]price{
	"gpt-6.1-sol": {in: 2, cacheRead: 0.10, out: 10},
	"gpt-6-luna":  {in: 0.10, cacheRead: 0.01, out: 0.50},
	"gpt-6-astra": {in: 10, cacheRead: 1, out: 50},
}

func finalizeCodexCost(_ []byte, _ func(Event), res *Result) {
	res.CostUSD, res.CostEstimate = 0, false
	if res.Usage == nil {
		res.Usage = &UsageMetadata{InputSemantics: "inclusive_cache", Status: "missing"}
	}
	res.Usage.CostStatus = "missing"
	p, ok := openAIPrices[datedSuffix.ReplaceAllString(res.Model, "")]
	if !ok || res.Usage.Status != "reported" {
		return
	}
	if _, known, _ := res.ModelTokens(); !known {
		return
	}
	// Codex input includes cache reads; bill those tokens only at the cached rate.
	res.CostUSD = (float64(res.InputTokens-res.CacheRead)*p.in +
		float64(res.CacheRead)*p.cacheRead + float64(res.OutputTokens)*p.out) / 1e6
	res.CostEstimate = true
	res.Usage.CostStatus = "estimated"
}
