// Package report summarizes run and batch records.
package report

import (
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/purpose"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// ByBackend and peers name backend, model, dir, purpose, routed and day grouping keys.
const (
	ByBackend = "backend"
	ByModel   = "model"
	ByDir     = "dir"
	ByPurpose = "purpose"
	ByRouted  = "routed"
	ByDay     = "day"
)

// ContextFromTurns and peers name turns, totals and mixed sources.
const (
	ContextFromTurns  = "turns"
	ContextFromTotals = "totals"
	ContextFromMixed  = "mixed"
)

// ParseSince reads a duration with day support; empty means no window.
func ParseSince(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, fmt.Errorf("invalid window %q (want e.g. 7d, 36h or 90m)", s)
		}
		if n > float64(math.MaxInt64)/float64(24*time.Hour) {
			return 0, fmt.Errorf("invalid window %q: too large", s)
		}
		d = time.Duration(n * float64(24*time.Hour))
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return 0, fmt.Errorf("invalid window %q (want e.g. 7d, 36h or 90m)", s)
		}
	}
	if d < 0 {
		return 0, fmt.Errorf("invalid window %q: must not be negative", s)
	}
	return d, nil
}

// Data holds records since Since, or all when zero.
type Data struct {
	Since time.Time
	Runs  []runlog.Record
	// Unfinished counts runs without a result (running or killed); no table includes them.
	Unfinished int
	Batches    []batch.Result
	// RunningBatches counts excluded in-progress batches.
	RunningBatches int

	Unreadable int

	Turns map[string][]runlog.Turn
}

// Collect reads every run and batch record under HRN_HOME.
func Collect(since time.Time) (Data, error) {
	recs, unreadableRuns, err := runlog.ListSince(since)
	if err != nil {
		return Data{}, fmt.Errorf("reading runs: %w", err)
	}
	batches, unreadableBatches, err := batch.List()
	if err != nil {
		return Data{}, fmt.Errorf("reading batches: %w", err)
	}
	d := Select(recs, batches, since)
	d.Unreadable = unreadableRuns + unreadableBatches
	// A run whose events cannot be read is reported from its totals.
	d.Turns = map[string][]runlog.Turn{}
	for _, r := range d.Runs {
		if turns, err := runlog.TurnsOf(r.ID); err == nil && len(turns) > 0 {
			d.Turns[r.ID] = turns
		}
	}
	return d, nil
}

// Select filters by window and completion.
func Select(recs []runlog.Record, batches []batch.Result, since time.Time) Data {
	d := Data{Since: since}
	for _, r := range recs {
		if r.Started.Before(since) {
			continue
		}
		if !r.Done {
			d.Unfinished++
			continue
		}
		d.Runs = append(d.Runs, r)
	}
	for _, b := range batches {
		switch {
		case b.Started.Before(since):
		case running(b):
			d.RunningBatches++
		default:
			d.Batches = append(d.Batches, b)
		}
	}
	return d
}

func running(b batch.Result) bool {
	for _, t := range b.Tasks {
		if t.Status == batch.StatusRunning {
			return true
		}
	}
	return false
}

// Report is everything hrn report prints, in the shape --json emits.
type Report struct {
	Since *time.Time `json:"since"` // nil when the report covers all time
	By    string     `json:"by"`

	Runs       []Row `json:"runs"`
	RunsAll    Row   `json:"runs_all"`
	Unfinished int   `json:"unfinished"`

	Batches        []BatchRow `json:"batches"`
	BatchesAll     BatchRow   `json:"batches_all"`
	RunningBatches int        `json:"running_batches"`
	Unreadable     int        `json:"unreadable"`
}

// Report builds both tables. An empty key groups the runs by backend.
func (d Data) Report(by string) (Report, error) {
	if by == "" {
		by = ByBackend
	}
	rows, all, err := d.RunRows(by)
	if err != nil {
		return Report{}, err
	}
	brows, ball := d.BatchRows()
	rep := Report{By: by, Runs: rows, RunsAll: all, Unfinished: d.Unfinished,
		Batches: brows, BatchesAll: ball, RunningBatches: d.RunningBatches, Unreadable: d.Unreadable}
	if !d.Since.IsZero() {
		since := d.Since
		rep.Since = &since
	}
	return rep, nil
}

// Row summarizes finished runs sharing a key.
type Row struct {
	Key            string        `json:"key"`
	Runs           int           `json:"runs"`
	OK             int           `json:"ok"`
	MedianDuration time.Duration `json:"median_duration_ns"`
	MedianTurns    float64       `json:"median_turns"`
	TotalCost      float64       `json:"total_cost_usd"`
	MedianCost     float64       `json:"median_cost_usd"`

	RunsWithoutCost int `json:"runs_without_cost,omitempty"`
	// CostEstimated includes any list-price estimate.
	CostEstimated bool `json:"cost_is_estimate,omitempty"`
	// CacheShare is reads / all input (including writes), or -1 when unknown.
	CacheShare float64 `json:"cache_share"`

	InputTokens       int64 `json:"input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	CacheRead         int64 `json:"cache_read_tokens"`
	CacheWrite        int64 `json:"cache_write_tokens"`
	RunsWithoutTokens int   `json:"runs_without_tokens,omitempty"`
	// MedianContext and PeakContext use tokens per call, or run averages.
	// ContextFrom is turns, totals or mixed; empty means no reported tokens.
	MedianContext float64 `json:"median_context_tokens"`
	PeakContext   int64   `json:"peak_context_tokens"`
	ContextFrom   string  `json:"context_from,omitempty"`
}

// RunRows groups runs, sorting days by key and others by size then key.
func (d Data) RunRows(by string) (rows []Row, all Row, err error) {
	key, err := keyFunc(by)
	if err != nil {
		return nil, Row{}, err
	}
	rows = []Row{} // never nil, so --json prints [] for an empty table
	groups := map[string][]runlog.Record{}
	for _, r := range d.Runs {
		k := key(r)
		groups[k] = append(groups[k], r)
	}
	for k, recs := range groups {
		rows = append(rows, summarize(k, recs, d.Turns))
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Runs != rows[j].Runs && by != ByDay {
			return rows[i].Runs > rows[j].Runs
		}
		return rows[i].Key < rows[j].Key
	})
	return rows, summarize("all", d.Runs, d.Turns), nil
}

func keyFunc(by string) (func(runlog.Record) string, error) {
	switch by {
	case ByBackend:
		return func(r runlog.Record) string { return r.Backend }, nil
	case ByModel:
		// Prefer the answering model over the requested model.
		return func(r runlog.Record) string {
			for _, m := range []string{r.Result.Model, r.Task.Model} {
				if m != "" {
					return m
				}
			}
			return "(unknown)"
		}, nil
	case ByDir:
		seen := map[string]string{}
		return func(r runlog.Record) string {
			k, ok := seen[r.Task.Dir]
			if !ok {
				k = canonDir(r.Task.Dir)
				seen[r.Task.Dir] = k
			}
			return k
		}, nil
	case ByPurpose:
		return func(r runlog.Record) string { return purpose.Resolve(r.Task.Purpose, r.Task.Prompt).Category }, nil
	case ByRouted:
		return func(r runlog.Record) string {
			if r.RoutedBy == nil {
				return "-"
			}
			eval := r.RoutedBy.Eval
			if eval == "" {
				eval = "hand"
			}
			return r.RoutedBy.Category + " via " + eval
		}, nil
	case ByDay:
		return func(r runlog.Record) string { return r.Started.Local().Format("2006-01-02") }, nil
	}
	return nil, fmt.Errorf("unknown key %q (want backend, model, dir, purpose, routed or day)", by)
}

// Resolve Windows 8.3 aliases to avoid duplicate groups; retain missing paths.
func canonDir(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return filepath.Clean(p)
}

func summarize(key string, recs []runlog.Record, turnsOf map[string][]runlog.Turn) Row {
	row := Row{Key: key, Runs: len(recs), CacheShare: -1}
	var durations, turns, costs, contexts, runAverages []float64
	var input, cached int64
	fromTurns, fromTotals := false, false
	for _, r := range recs {
		res := r.Result
		if res.Backend == "" {
			res.Backend = r.Backend
		}
		if res.UsageMissing() || res.InputTokens > math.MaxInt64-row.InputTokens || res.OutputTokens > math.MaxInt64-row.OutputTokens ||
			res.CacheRead > math.MaxInt64-row.CacheRead || res.CacheWrite > math.MaxInt64-row.CacheWrite {
			row.RunsWithoutTokens++
		} else {
			row.InputTokens += res.InputTokens
			row.OutputTokens += res.OutputTokens
			row.CacheRead += res.CacheRead
			row.CacheWrite += res.CacheWrite
		}
		if calls := turnsOf[r.ID]; len(calls) > 0 {
			fromTurns = true
			var sum float64
			for _, c := range calls {
				contexts = append(contexts, float64(c.ContextTokens))
				sum += float64(c.ContextTokens)
				row.PeakContext = max(row.PeakContext, c.ContextTokens)
			}
			runAverages = append(runAverages, sum/float64(len(calls)))
		} else if total, known, _ := res.ModelTokens(); known && total > res.OutputTokens && res.Turns > 0 {
			// Missing calls require an average; the last context may exceed it.
			fromTotals = true
			avg := (total - res.OutputTokens) / int64(res.Turns)
			runAverages = append(runAverages, float64(avg))
			row.PeakContext = max(row.PeakContext, avg, res.ContextTokens)
		}
		if res.Status == agent.StatusOK {
			row.OK++
		}
		durations = append(durations, float64(res.Duration))
		turns = append(turns, float64(res.Turns))
		if cost, known, estimated := res.KnownCost(); known {
			costs = append(costs, cost)
			row.TotalCost += cost
			row.CostEstimated = row.CostEstimated || estimated
		} else {
			row.RunsWithoutCost++
		}
		// Normalize overlapping and disjoint cache counters before computing share.
		if total, known, _ := res.ModelTokens(); known {
			input += total - res.OutputTokens - res.CacheRead
			cached += res.CacheRead
		} else if res.Usage == nil {
			input += res.InputTokens + res.CacheWrite
			cached += res.CacheRead
		}
	}
	row.MedianDuration = time.Duration(Median(durations))
	row.MedianTurns = Median(turns)
	row.MedianCost = Median(costs)
	row.MedianContext = Median(runAverages)
	if fromTurns && !fromTotals {
		row.MedianContext = Median(contexts)
	}
	switch {
	case fromTurns && fromTotals:
		row.ContextFrom = ContextFromMixed
	case fromTurns:
		row.ContextFrom = ContextFromTurns
	case fromTotals:
		row.ContextFrom = ContextFromTotals
	}
	if input+cached > 0 {
		row.CacheShare = float64(cached) / float64(input+cached)
	}
	return row
}

// BatchRow includes all attempt costs; the attempts median excludes unstarted tasks.
type BatchRow struct {
	Key            string  `json:"key"`
	Batches        int     `json:"batches"`
	Tasks          int     `json:"tasks"`
	Passed         int     `json:"passed"`
	MedianAttempts float64 `json:"median_attempts"`
	TotalCost      float64 `json:"total_cost_usd"`
	CostEstimated  bool    `json:"cost_is_estimate,omitempty"`
	// TasksWithoutCost counts started tasks with no known attempt cost.
	TasksWithoutCost int `json:"tasks_without_cost,omitempty"`
}

// BatchRows groups the batches by repository, plus an "all" row.
func (d Data) BatchRows() (rows []BatchRow, all BatchRow) {
	rows = []BatchRow{}
	groups := map[string][]batch.Result{}
	for _, b := range d.Batches {
		k := canonDir(b.Dir)
		groups[k] = append(groups[k], b)
	}
	for k, bs := range groups {
		rows = append(rows, summarizeBatches(k, bs))
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Batches != rows[j].Batches {
			return rows[i].Batches > rows[j].Batches
		}
		return rows[i].Key < rows[j].Key
	})
	return rows, summarizeBatches("all", d.Batches)
}

func summarizeBatches(key string, bs []batch.Result) BatchRow {
	row := BatchRow{Key: key, Batches: len(bs)}
	var attempts []float64
	for _, b := range bs {
		for _, t := range b.Tasks {
			row.Tasks++
			if t.Status == batch.StatusPass {
				row.Passed++
			}
			// Unstarted tasks provide no evidence about the attempts needed.
			if n := len(t.Attempts); n > 0 {
				attempts = append(attempts, float64(n))
			}
			costKnown := false
			for _, a := range t.Attempts {
				if cost, known, estimated := a.Result.KnownCost(); known {
					costKnown = true
					row.TotalCost += cost
					row.CostEstimated = row.CostEstimated || estimated
				}
			}
			if len(t.Attempts) > 0 && !costKnown {
				row.TasksWithoutCost++
			}
		}
	}
	row.MedianAttempts = Median(attempts)
	return row
}

// Median returns zero for no values, averaging an even middle pair.
func Median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// Percent renders a ratio or "-", never rounding partial success to all or nothing.
func Percent(part, whole int) string {
	if whole == 0 {
		return "-"
	}
	p := int(math.Round(100 * float64(part) / float64(whole)))
	switch {
	case p == 100 && part < whole:
		p = 99
	case p == 0 && part > 0:
		p = 1
	}
	return strconv.Itoa(p) + "%"
}

// Money renders dollars, "~" for estimates and "-" for zero.
func Money(v float64, estimated bool) string {
	if v == 0 {
		return "-"
	}
	prefix := ""
	if estimated {
		prefix = "~"
	}
	if v < 0.01 {
		return fmt.Sprintf("%s$%.4f", prefix, v)
	}
	return fmt.Sprintf("%s$%.2f", prefix, v)
}

// KnownMoney distinguishes unknown ("-") from zero ("$0.00").
func KnownMoney(v float64, known, estimated bool) string {
	switch {
	case !known:
		return "-"
	case v == 0 && estimated:
		return "~$0.00"
	case v == 0:
		return "$0.00"
	}
	return Money(v, estimated)
}

// Number renders a median without trailing zeros: 9, not 9.0; 6.5 stays.
func Number(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// Bytes renders a size in the largest unit that keeps it above one.
func Bytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// Share renders a ratio as a percentage, or "-" for the unknown value.
func Share(ratio float64) string {
	if ratio < 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", 100*ratio)
}

// Tokens renders a token count as hrn's summaries do: 340, 1.2k, 3.4M.
func Tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return strconv.FormatInt(n, 10)
}
