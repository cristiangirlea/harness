package watch

import (
	"fmt"
	"slices"
	"strings"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// The usage chart is drawn here and rendered by the "usage" template as
// inline SVG: no script draws it and no style attribute colours it, so it
// lives within the page's policy, and html/template escapes every text in it.
// Two panels share the turn axis, one scale each: the context a turn carried,
// and the cache reads up to it, which only grow.

const (
	chartW, chartH  = 640, 252
	chartLeft       = 58
	chartRight      = 14
	panelH          = 78
	contextTop      = 30
	cacheTop        = 148
	chartAxisLabelY = 246
)

type usageView struct {
	Turns   int
	Summary string
	Label   string // the chart's accessible name
	W, H    int
	Left    int
	Right   int // the plot's right edge
	First   string
	Last    string
	AxisY   int
	Panels  []panelView
	Marks   []markView
	Keyed   bool // a compaction is marked, so the key is shown
	Rows    []turnRow
}

type panelView struct {
	Title       string
	Max         string
	TitleY      int
	Top, Base   int
	MaxY, ZeroY int
	Points      string
	Dots        []dotView
}

type dotView struct {
	X, Y string
	Tip  string
}

type markView struct {
	X        string
	Top, Bot int
	Probable bool
	Tip      string
}

type turnRow struct {
	N                                             int
	Model                                         string
	Input, Output, CacheRead, CacheWrite, Context int64
	SoFar                                         int64
	Note                                          string
}

// isUsage reports whether an event belongs to the chart rather than the
// timeline: a usage event is drawn, never listed.
func isUsage(l Line) bool { return l.Err == nil && l.Event.Kind == agent.EventUsage }

// feedsChart reports whether an event changes the chart.
func feedsChart(l Line) bool {
	return l.Err == nil && (l.Event.Kind == agent.EventUsage || l.Event.Kind == agent.EventCompaction)
}

// usageOf draws a run's turns. A run without usage events has no turns and
// the template draws nothing.
func usageOf(events []agent.Event) usageView {
	turns := runlog.Turns(events)
	v := usageView{Turns: len(turns), W: chartW, H: chartH, Left: chartLeft, Right: chartW - chartRight, AxisY: chartAxisLabelY}
	if len(turns) == 0 {
		return v
	}
	var peak, reads int64
	var contexts []int64
	recorded, probable := 0, 0
	for _, t := range turns {
		peak = max(peak, t.ContextTokens)
		contexts = append(contexts, t.ContextTokens)
		switch t.Compaction {
		case runlog.CompactionRecorded:
			recorded++
		case runlog.CompactionProbable:
			probable++
		}
	}
	reads = turns[len(turns)-1].CacheReadSoFar
	slices.Sort(contexts)
	median := contexts[len(contexts)/2]
	if len(contexts)%2 == 0 {
		median = (contexts[len(contexts)/2-1] + median) / 2
	}
	plural := "s"
	if len(turns) == 1 {
		plural = ""
	}
	parts := []string{fmt.Sprintf("%d turn%s", len(turns), plural), "peak context " + tokens(peak), "median " + tokens(median), tokens(reads) + " cache read"}
	if recorded > 0 {
		parts = append(parts, fmt.Sprintf("%d compacted", recorded))
	}
	if probable > 0 {
		parts = append(parts, fmt.Sprintf("%d probably compacted", probable))
	}
	v.Summary = strings.Join(parts, " · ")
	v.Label = "Context per turn and cache reads so far over " + v.Summary
	v.First, v.Last = "turn 1", fmt.Sprintf("turn %d", len(turns))

	plotW := float64(chartW - chartLeft - chartRight)
	x := func(n int) float64 {
		if len(turns) == 1 {
			return chartLeft + plotW/2
		}
		return chartLeft + plotW*float64(n-1)/float64(len(turns)-1)
	}
	panel := func(title string, top int, top0 int64, value func(runlog.Turn) int64, tip func(runlog.Turn) string) panelView {
		p := panelView{Title: title, Max: tokens(top0), TitleY: top - 12, Top: top, Base: top + panelH, MaxY: top + 4, ZeroY: top + panelH}
		var pts []string
		for _, t := range turns {
			y := float64(top + panelH)
			if top0 > 0 {
				y -= float64(panelH) * float64(value(t)) / float64(top0)
			}
			xs, ys := fmt.Sprintf("%.1f", x(t.N)), fmt.Sprintf("%.1f", y)
			pts = append(pts, xs+","+ys)
			p.Dots = append(p.Dots, dotView{X: xs, Y: ys, Tip: tip(t)})
		}
		p.Points = strings.Join(pts, " ")
		return p
	}
	v.Panels = []panelView{
		panel("Context per turn", contextTop, peak, func(t runlog.Turn) int64 { return t.ContextTokens }, func(t runlog.Turn) string {
			tip := fmt.Sprintf("turn %d · context %s", t.N, tokens(t.ContextTokens))
			if t.Model != "" {
				tip += " · " + t.Model
			}
			return tip
		}),
		panel("Cache reads so far", cacheTop, reads, func(t runlog.Turn) int64 { return t.CacheReadSoFar }, func(t runlog.Turn) string {
			return fmt.Sprintf("turn %d · %s cache read so far", t.N, tokens(t.CacheReadSoFar))
		}),
	}
	for i, t := range turns {
		row := turnRow{N: t.N, Model: t.Model, Input: t.InputTokens, Output: t.OutputTokens, CacheRead: t.CacheRead,
			CacheWrite: t.CacheWrite, Context: t.ContextTokens, SoFar: t.CacheReadSoFar}
		if t.Compaction != "" {
			// Between this turn and the one before, where the context dropped.
			mx := x(t.N)
			if i > 0 {
				mx = (x(t.N) + x(turns[i-1].N)) / 2
			}
			m := markView{X: fmt.Sprintf("%.1f", mx), Top: contextTop, Bot: cacheTop + panelH, Probable: t.Compaction == runlog.CompactionProbable}
			if m.Probable {
				m.Tip = fmt.Sprintf("probably compacted before turn %d: the context fell from %s to %s", t.N, tokens(turns[i-1].ContextTokens), tokens(t.ContextTokens))
			} else {
				m.Tip = fmt.Sprintf("compacted before turn %d", t.N)
				if t.Trigger != "" {
					m.Tip += " (" + t.Trigger + ")"
				}
				if t.CompactedFrom > 0 {
					m.Tip += ", from " + tokens(t.CompactedFrom)
				}
			}
			row.Note = m.Tip
			v.Marks = append(v.Marks, m)
		}
		v.Rows = append(v.Rows, row)
	}
	v.Keyed = len(v.Marks) > 0
	return v
}

// usageEvents keeps what the chart is drawn from.
func usageEvents(lines []Line) []agent.Event {
	var out []agent.Event
	for _, l := range lines {
		if feedsChart(l) {
			out = append(out, l.Event)
		}
	}
	return out
}
