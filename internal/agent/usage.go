package agent

import (
	"math"

	"github.com/cristiangirlea/harness/internal/kernel"
)

type UsageMetadata = kernel.UsageMetadata

func addUsage(r *Result, semantics string, input, output, cacheRead, cacheWrite int64) bool {
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
