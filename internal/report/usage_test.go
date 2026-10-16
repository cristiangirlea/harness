package report

import (
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestRunRowsByDayGoInDateOrder(t *testing.T) {
	at := func(day, hour int) runlog.Record {
		started := time.Date(2026, 9, day, hour, 0, 0, 0, time.Local)
		return runlog.Record{ID: started.Format("0102-15"), Backend: "claude", Started: started, Done: true,
			Result: agent.Result{Status: agent.StatusOK}}
	}
	d := Select([]runlog.Record{at(23, 9), at(21, 23), at(21, 0), at(23, 18), at(23, 20), at(22, 12)}, nil, time.Time{})
	rows, all, err := d.RunRows(ByDay)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		key  string
		runs int
	}{{"2026-09-21", 2}, {"2026-09-22", 1}, {"2026-09-23", 3}}
	if len(rows) != len(want) || all.Runs != 6 {
		t.Fatalf("rows = %+v, all = %+v", rows, all)
	}
	for i, w := range want {
		if rows[i].Key != w.key || rows[i].Runs != w.runs {
			t.Errorf("row %d = %s with %d runs, want %s with %d", i, rows[i].Key, rows[i].Runs, w.key, w.runs)
		}
	}
}

// Context per turn comes from a run's usage events when it has them, from its
// totals otherwise (one average a run), and a row says which. A row with both
// takes one average a run from each, never turns and averages together.
func TestRunRowsSumTokensAndReadContextFromTurnsOrTotals(t *testing.T) {
	withTurns := rec("api", "m", "/r", 0, agent.Result{Status: agent.StatusOK, Turns: 3,
		InputTokens: 300, OutputTokens: 60, CacheRead: 80000, CacheWrite: 9700, ContextTokens: 50000})
	fromTotals := rec("claude", "m", "/r", time.Hour, agent.Result{Status: agent.StatusOK, Turns: 2,
		InputTokens: 1000, OutputTokens: 40, CacheRead: 39000, ContextTokens: 25000})
	noTokens := rec("codex", "m", "/r", 2*time.Hour, agent.Result{Status: agent.StatusOK, Turns: 4})
	d := Select([]runlog.Record{withTurns, fromTotals, noTokens}, nil, time.Time{})
	d.Turns = map[string][]runlog.Turn{withTurns.ID: {
		{N: 1, Usage: agent.Usage{ContextTokens: 10000}},
		{N: 2, Usage: agent.Usage{ContextTokens: 10000}},
		{N: 3, Usage: agent.Usage{ContextTokens: 70000}},
	}}
	rows, all, err := d.RunRows(ByBackend)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]Row{}
	for _, r := range rows {
		byKey[r.Key] = r
	}
	for _, tc := range []struct {
		row          Row
		from         string
		median       float64
		peak         int64
		input, reads int64
	}{
		{byKey["api"], ContextFromTurns, 10000, 70000, 300, 80000},
		// (1000 + 39000) / 2 turns; the peak is the last call's 25000, above that average.
		{byKey["claude"], ContextFromTotals, 20000, 25000, 1000, 39000},
		{byKey["codex"], "", 0, 0, 0, 0},
		// The api run's average, 30000, and the claude run's, 20000.
		{all, ContextFromMixed, 25000, 70000, 1300, 119000},
	} {
		r := tc.row
		if r.ContextFrom != tc.from || r.MedianContext != tc.median || r.PeakContext != tc.peak || r.InputTokens != tc.input || r.CacheRead != tc.reads {
			t.Errorf("%s: from %q median %v peak %d input %d reads %d; want %q %v %d %d %d",
				r.Key, r.ContextFrom, r.MedianContext, r.PeakContext, r.InputTokens, r.CacheRead, tc.from, tc.median, tc.peak, tc.input, tc.reads)
		}
	}
	if all.OutputTokens != 100 || all.CacheWrite != 9700 {
		t.Errorf("all = %+v", all)
	}
}
