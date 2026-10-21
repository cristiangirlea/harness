// Package route builds purpose routing tables from measured eval results.
package route

import (
	"math"
	"slices"

	"github.com/cristiangirlea/harness/internal/eval"
	"github.com/cristiangirlea/harness/internal/purpose"
	"github.com/cristiangirlea/harness/internal/route/table"
	"github.com/cristiangirlea/harness/internal/runlog"
)

type Row = table.Row
type Table = table.Table
type RoutedBy = table.RoutedBy

func Load() (Table, error) { return table.Load(runlog.Home()) }
func Save(t Table) error   { return table.Save(runlog.Home(), t) }

// FromEval picks qualifying family efforts by eval-wide median cost, then purpose winners.
// Ties prefer plan CLIs, lower effort, then name for determinism.
func FromEval(rec eval.Record, margin float64) Table {
	out := Table{}
	if math.IsNaN(margin) || math.IsInf(margin, 0) || margin < 0 || margin > 1 {
		return out
	}
	// Routing cannot reproduce harness changes, so exclude their scores.
	if rec.Spec.AppendSystemPrompt != "" || rec.Spec.NoSkills {
		return out
	}
	categories := map[string]string{}
	tasks := map[string]struct{ backend, model, effort string }{}
	for _, t := range rec.Spec.Tasks {
		if t.AppendSystemPrompt != "" || t.NoSkills {
			continue
		}
		categories[t.ID] = purpose.Classify(t.Prompt).Category
		tasks[t.ID] = struct{ backend, model, effort string }{t.Backend, t.Model, t.Effort}
	}
	// Left out of the bar too, so a better harness does not disqualify every plain variant.
	harnessed := map[string]bool{}
	for _, v := range rec.Spec.Matrix {
		harnessed[v.Key()] = v.ChangesHarness()
	}
	groups := map[string]map[string]eval.VariantRow{}
	for _, t := range rec.Table.Tasks {
		cat, ok := categories[t.Task]
		if !ok || t.Total-t.DidNotAct <= 0 || harnessed[t.Variant] {
			continue
		}
		if groups[cat] == nil {
			groups[cat] = map[string]eval.VariantRow{}
		}
		r := groups[cat][t.Variant]
		r.Name = t.Variant
		r.Attempts += t.Total
		r.DidNotAct += t.DidNotAct
		r.Passed += t.Passed
		groups[cat][t.Variant] = r
	}
	type candidate struct {
		v    eval.Variant
		name string
		cost float64
	}
	less := func(a, b candidate) bool {
		if a.cost != b.cost {
			return a.cost < b.cost
		}
		plan := func(b string) bool { return b == "claude" || b == "codex" }
		if plan(a.v.Backend) != plan(b.v.Backend) {
			return plan(a.v.Backend)
		}
		levels := []string{"low", "medium", "high", "xhigh", "max", ""}
		if a.v.Effort != b.v.Effort {
			return slices.Index(levels, a.v.Effort) < slices.Index(levels, b.v.Effort)
		}
		return a.name < b.name
	}
	costs := map[string]float64{}
	for _, v := range rec.Table.Variants {
		costs[v.Name] = v.MedianCostUSD
	}
	for cat, rows := range groups {
		best := 0.0
		for _, r := range rows {
			best = max(best, r.PassRate())
		}
		families := map[string]candidate{}
		for _, v := range rec.Spec.Matrix {
			name := v.Key()
			r, ok := rows[name]
			if !ok || r.Attempts <= r.DidNotAct || r.PassRate()+margin+1e-12 < best {
				continue
			}
			cost, ok := costs[name]
			if !ok || cost < 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
				continue
			}
			// Only uniform variant-over-task-over-spec choices fit a single routing row.
			first := func(values ...string) string {
				for _, s := range values {
					if s != "" {
						return s
					}
				}
				return ""
			}
			var effective eval.Variant
			found, mixed := false, false
			for _, tr := range rec.Table.Tasks {
				if tr.Variant != name || tr.Total-tr.DidNotAct <= 0 || categories[tr.Task] != cat {
					continue
				}
				t := tasks[tr.Task]
				resolved := v
				resolved.Backend = first(v.Backend, t.backend, rec.Spec.Backend)
				resolved.Model = first(v.Model, t.model, rec.Spec.Model)
				resolved.Effort = first(v.Effort, t.effort, rec.Spec.Effort)
				if found && resolved != effective {
					mixed = true
					break
				}
				effective, found = resolved, true
			}
			if mixed || !found || effective.Backend == "" {
				continue
			}
			v = effective
			c := candidate{v, name, cost}
			key := v.Backend + "\x00" + v.Model
			if prev, ok := families[key]; !ok || less(c, prev) {
				families[key] = c
			}
		}
		var winner candidate
		found := false
		for _, c := range families {
			if !found || less(c, winner) {
				winner = c
				found = true
			}
		}
		if found {
			out[cat] = Row{Backend: winner.v.Backend, Model: winner.v.Model, Effort: winner.v.Effort, Eval: rec.ID, Set: rec.Started.Format("2006-01-02")}
		}
	}
	return out
}
