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

// maxRunUSD is more than any one run costs: a larger CostUSD is a corrupted
// record, and would overflow the sum in 1e-8 USD units.
const maxRunUSD = 1e6

// purposeSum adds up the runs of one row.
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

// lowerBound prints a partial sum of 1e-8 USD units rounded down to four
// decimals, so that it never claims more than is known; a floored zero is
// still a number. The sum is exact, so 0.1+0.7 floors to 0.8000, not 0.7999.
func lowerBound(usd int64, estimate bool) string {
	str := fmt.Sprintf("$%d.%04d", usd/1e8, usd/1e4%1e4)
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
