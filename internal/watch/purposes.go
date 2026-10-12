package watch

import (
	"fmt"

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
	s.tokens += res.InputTokens + res.OutputTokens + res.CacheRead
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
		r.Cost = "≥ " + r.Cost
	}
	return r
}

// purposesOf builds the card over the runs the index lists; nil for none.
func purposesOf(runs []runlog.Record) *purposeCard {
	if len(runs) == 0 {
		return nil
	}
	by := map[string]*purposeSum{}
	var all purposeSum
	for _, rec := range runs {
		c := purpose.Classify(rec.Task.Prompt).Category
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
