package main

import (
	"fmt"
	"strconv"

	"github.com/cristiangirlea/harness/internal/eval"
	"github.com/cristiangirlea/harness/internal/kernel"
)

func printToolStats(r *renderer, label string, a, b map[string]kernel.ToolStat, diff bool) {
	pairs := eval.ToolStatPairs(a, b)
	if len(pairs) == 0 {
		return
	}
	r.Line("")
	r.Line("  tool stats: " + label)
	r.Line(fmt.Sprintf("  %-24s %-16s %-16s %s", "tool", "calls", "errors", "bytes"))
	for _, p := range pairs[:min(10, len(pairs))] {
		calls, errors, bytes := strconv.Itoa(p.A.Calls), strconv.Itoa(p.A.Errors), strconv.Itoa(p.A.ResultBytes)
		if diff {
			calls, errors, bytes = "-", "-", "-"
			if len(a) > 0 {
				calls, errors, bytes = strconv.Itoa(p.A.Calls), strconv.Itoa(p.A.Errors), strconv.Itoa(p.A.ResultBytes)
			}
			bc, be, bb := "-", "-", "-"
			if len(b) > 0 {
				bc, be, bb = strconv.Itoa(p.B.Calls), strconv.Itoa(p.B.Errors), strconv.Itoa(p.B.ResultBytes)
			}
			calls, errors, bytes = arrow(calls, bc), arrow(errors, be), arrow(bytes, bb)
		}
		name := printable(p.Name)
		if name == "" {
			name = `""`
		}
		r.Line(fmt.Sprintf("  %-24s %-16s %-16s %s", name, calls, errors, bytes))
	}
	if len(pairs) > 10 {
		r.Line(fmt.Sprintf("  (%d more tools)", len(pairs)-10))
	}
}
