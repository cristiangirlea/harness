package watch

import (
	"fmt"
	"math"

	"github.com/cristiangirlea/harness/internal/purpose"
	"github.com/cristiangirlea/harness/internal/runlog"
)

type purposeCard struct {
	Scope   string
	Partial bool // some finished run reported no cost
	Rows    []purposeRow
}

// Missing costs are unknown, not zero: Cost is a lower bound while NoCost is nonzero.
type purposeRow struct {
	Purpose               string
	Runs, Running, Tokens string
	Cost, NoCost          string
}

// Reject corrupted costs that could overflow the sum in 1e-8 USD units.
const maxRunUSD = 1e6

type purposeSum struct {
	runs, running, noCost int
	tokens                int64
	usd                   int64 // in 1e-8 USD, so that the sum is exact
	known, estimate       bool
}

func (s *purposeSum) add(rec runlog.Record) {
	s.runs++
	if !rec.Done {
		if runStopped(rec).IsZero() {
			s.running++
		}
		return
	}
	res := rec.Result
	if res.Backend == "" {
		res.Backend = rec.Backend
	}
	if total, known, _ := res.ModelTokens(); known {
		s.tokens += total
	} else if res.Usage == nil {
		s.tokens += res.InputTokens + res.OutputTokens + res.CacheRead + res.CacheWrite
	}
	if res.CostUSD <= 0 || res.CostUSD > maxRunUSD {
		s.noCost++
		return
	}
	s.usd += int64(math.Round(res.CostUSD * 1e8))
	s.known = true
	s.estimate = s.estimate || res.CostEstimate
}

func (s purposeSum) row(name string) purposeRow {
	r := purposeRow{Purpose: name, Runs: fmt.Sprint(s.runs), Running: fmt.Sprint(s.running), Tokens: tokens(s.tokens), Cost: "-"}
	if s.noCost > 0 {
		r.NoCost = fmt.Sprintf("%d runs report no cost", s.noCost)
		if s.noCost == 1 {
			r.NoCost = "1 run reports no cost"
		}
	}
	if !s.known {
		return r
	}
	if s.noCost > 0 {
		r.Cost = "≥ " + lowerBound(s.usd, s.estimate)
	} else if s.usd == 0 {
		r.Cost = lowerBound(0, s.estimate) // known, only too small to round to a unit
	} else {
		r.Cost = cost(float64(s.usd)/1e8, s.estimate)
	}
	return r
}

// Floor exact 1e-8 USD units to four decimals so partial totals never overstate known cost.
func lowerBound(usd int64, estimate bool) string {
	str := fmt.Sprintf("$%d.%04d", usd/1e8, usd/1e4%1e4)
	if estimate {
		return "~" + str
	}
	return str
}

func purposesOf(runs []runlog.Record) *purposeCard {
	if len(runs) == 0 {
		return nil
	}
	by := map[string]*purposeSum{}
	var all purposeSum
	for _, rec := range runs {
		c := purpose.Resolve(rec.Task.Purpose, rec.Task.Prompt).Category
		if by[c] == nil {
			by[c] = &purposeSum{}
		}
		by[c].add(rec)
		all.add(rec)
	}
	card := &purposeCard{Scope: fmt.Sprintf("The newest %d runs, the ones listed below, not all time.", len(runs))}
	if len(runs) == 1 {
		card.Scope = "The newest run, the one listed below, not all time."
	}
	for _, c := range purpose.Categories {
		if s := by[c]; s != nil {
			card.Rows = append(card.Rows, s.row(c))
		}
	}
	card.Rows = append(card.Rows, all.row("all"))
	card.Partial = all.noCost > 0
	return card
}
