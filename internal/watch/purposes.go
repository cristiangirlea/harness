package watch

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/cristiangirlea/harness/internal/purpose"
	"github.com/cristiangirlea/harness/internal/runlog"
)

type purposeCard struct {
	Scope   string
	Partial bool // some finished run reported no cost
	Rows    []purposeRow
	Repos   []repoRow
}

// repoRow is a purposeRow named after a repository, with the purposes its runs had.
type repoRow struct {
	Row purposeRow
	Mix string
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

func (s *purposeSum) merge(o purposeSum) {
	s.runs += o.runs
	s.running += o.running
	s.noCost += o.noCost
	s.tokens += o.tokens
	s.usd += o.usd
	s.known = s.known || o.known
	s.estimate = s.estimate || o.estimate
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

// repoRows is how many repositories get a row of their own; the rest share one.
const repoRows = 10

type repoSum struct {
	key  string
	name string
	sum  purposeSum
	mix  map[string]int // runs per purpose
}

func cardOver(runs []tallied) *purposeCard {
	if len(runs) == 0 {
		return nil
	}
	by := map[string]*purposeSum{}
	repos := map[string]*repoSum{}
	var all purposeSum
	for _, r := range runs {
		if by[r.purpose] == nil {
			by[r.purpose] = &purposeSum{}
		}
		by[r.purpose].merge(r.sum)
		if repos[r.repoKey] == nil {
			repos[r.repoKey] = &repoSum{key: r.repoKey, name: r.repo, mix: map[string]int{}}
		}
		repos[r.repoKey].sum.merge(r.sum)
		repos[r.repoKey].mix[r.purpose]++
		all.merge(r.sum)
	}
	card := &purposeCard{Scope: fmt.Sprintf("All %d recorded runs, not only the ones listed below.", len(runs))}
	if len(runs) == 1 {
		card.Scope = "The one recorded run."
	}
	for _, c := range purpose.Categories {
		if s := by[c]; s != nil {
			card.Rows = append(card.Rows, s.row(c))
		}
	}
	card.Rows = append(card.Rows, all.row("all"))
	card.Partial = all.noCost > 0
	card.Repos = repoRowsOf(repos)
	return card
}

func repoRowsOf(repos map[string]*repoSum) []repoRow {
	list := make([]*repoSum, 0, len(repos))
	for _, r := range repos {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].sum.runs != list[j].sum.runs {
			return list[i].sum.runs > list[j].sum.runs
		}
		if list[i].name != list[j].name {
			return list[i].name < list[j].name
		}
		return list[i].key < list[j].key
	})
	if len(list) > repoRows+1 {
		rest := &repoSum{name: fmt.Sprintf("%d other folders", len(list)-repoRows), mix: map[string]int{}}
		for _, r := range list[repoRows:] {
			rest.sum.merge(r.sum)
			for p, n := range r.mix {
				rest.mix[p] += n
			}
		}
		list = append(list[:repoRows], rest)
	}
	// Number same-named checkouts rather than show more of their paths to a remote viewer.
	taken := map[string]bool{}
	for _, r := range list {
		taken[repoLabel(r.name)] = true
	}
	used := map[string]bool{}
	rows := make([]repoRow, len(list))
	for i, r := range list {
		name := repoLabel(r.name)
		for n := 2; used[name]; n++ {
			if next := fmt.Sprintf("%s (%d)", repoLabel(r.name), n); !taken[next] {
				name = next
			}
		}
		used[name], taken[name] = true, true
		rows[i] = repoRow{Row: r.sum.row(name), Mix: mixOf(r.mix)}
	}
	return rows
}

func repoLabel(name string) string {
	if name == "" {
		return "no folder"
	}
	return name
}

// mixOf lists purposes by how many runs had them, ties in category order.
func mixOf(mix map[string]int) string {
	cats := make([]string, 0, len(mix))
	for _, c := range purpose.Categories {
		if mix[c] > 0 {
			cats = append(cats, c)
		}
	}
	sort.SliceStable(cats, func(i, j int) bool { return mix[cats[i]] > mix[cats[j]] })
	parts := make([]string, len(cats))
	for i, c := range cats {
		parts[i] = fmt.Sprintf("%s %d", c, mix[c])
	}
	return strings.Join(parts, " · ")
}
