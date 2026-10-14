package watch

import (
	"fmt"
	"math"

	"github.com/cristiangirlea/harness/internal/purpose"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// purposeCard says what the listed runs were for and what they cost, one row
// per purpose that occurs and then all of them.
type purposeCard struct {
	Scope   string // which runs it covers
	Partial bool   // some finished run reported no cost
	Rows    []purposeRow
}

// purposeRow is ready to print. A finished run whose backend reported no cost
// is not $0: NoCost says how many did not, and while any did not, Cost is a
// lower bound ("≥") over those that did, or "-" when none did.
type purposeRow struct {
	Purpose               string
	Runs, Running, Tokens string
	Cost, NoCost          string
}

// purposeSum adds up the runs of one row.
type purposeSum struct {
	runs, running, noCost int
	tokens                int64
	usd                   float64
	known, estimate       bool
}

func (s *purposeSum) add(rec runlog.Record) {
	s.runs++
	if !rec.Done {
		s.running++
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
	if res.CostUSD <= 0 {
		s.noCost++
		return
	}
	s.usd += res.CostUSD
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
	r.Cost = cost(s.usd, s.estimate)
	if s.noCost > 0 {
		r.Cost = "≥ " + lowerBound(s.usd, s.estimate)
	}
	return r
}

// lowerBound prints a partial sum rounded down to four decimals, so that it
// never claims more than is known; a floored zero is still a number. The
// nudge keeps a sum that falls a hair short, such as 0.1+0.7, from flooring
// to 0.7999.
func lowerBound(usd float64, estimate bool) string {
	str := fmt.Sprintf("$%.4f", math.Floor(usd*1e4+1e-9)/1e4)
	if estimate {
		return "~" + str
	}
	return str
}

// purposesOf builds the card over the runs the index lists; nil for none.
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
