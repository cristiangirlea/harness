// Package eval is the scoring layer: one task set run across a matrix of
// backends, models and efforts, repeated, and judged by the same gates a batch
// uses and, optionally, by a model reading each diff against a rubric. It answers "which backend, model and effort for this kind of task"
// with numbers instead of impressions.
//
// An eval is a batch with two extra dimensions, so it expands into a
// batch.Spec and runs through batch.Run unchanged. Every cell is an ordinary
// recorded run.
package eval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/report"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// Spec is a batch spec plus the two dimensions that make it an eval: the
// matrix of variants to compare and how many times to repeat each cell.
type Spec struct {
	batch.Spec
	Matrix  []Variant `json:"matrix"`
	Repeats int       `json:"repeats,omitempty"` // default 1
	// Judge, when set, scores every passing cell's diff against a rubric.
	Judge *Judge `json:"judge,omitempty"`
}

// Variant is one cell of the matrix: what to run the task set with.
type Variant struct {
	Lean             *bool  `json:"lean,omitempty"`
	ShapeTools       bool   `json:"shape_tools,omitempty"`
	Retrieval        string `json:"retrieval,omitempty"`
	RetrievalVersion string `json:"retrieval_version,omitempty"`
	Name             string `json:"name,omitempty"` // default: backend[-model][-effort]
	Backend          string `json:"backend,omitempty"`
	Model            string `json:"model,omitempty"`
	Effort           string `json:"effort,omitempty"`
}

func (v Variant) name() string {
	if v.Name != "" {
		return v.Name
	}
	parts := []string{v.Backend}
	if v.Model != "" {
		parts = append(parts, v.Model)
	}
	if v.Effort != "" {
		parts = append(parts, v.Effort)
	}
	return strings.Join(parts, "-")
}

// LoadSpec reads an eval spec. A relative dir is taken from the file's
// directory, as in a batch spec.
func LoadSpec(path string) (Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Spec{}, err
	}
	var spec Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return Spec{}, fmt.Errorf("%s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return Spec{}, err
	}
	if !filepath.IsAbs(spec.Dir) {
		spec.Dir = filepath.Join(filepath.Dir(abs), spec.Dir)
	}
	return spec, nil
}

// Expand turns an eval spec into the batch that runs it: one task per task,
// variant and repeat. Retry is deliberately nil, and no_switch set, because an
// eval measures what a single attempt does; a retried attempt would score the
// harness, not the backend, and a switched one the other backend.
func Expand(spec Spec, evalID string) (batch.Spec, error) {
	if len(spec.Matrix) == 0 {
		return batch.Spec{}, fmt.Errorf("matrix is empty: an eval needs at least one variant to compare")
	}
	if spec.Repeats < 0 {
		return batch.Spec{}, fmt.Errorf("repeats %d is negative", spec.Repeats)
	}
	if spec.Repeats == 0 {
		spec.Repeats = 1
	}
	if len(spec.Tasks) == 0 {
		return batch.Spec{}, fmt.Errorf("no tasks")
	}
	if len(spec.Retry) > 0 {
		return batch.Spec{}, fmt.Errorf("retry does not belong in an eval: a cell measures one attempt, so a retried one would score the harness instead of the backend")
	}
	for _, t := range spec.Tasks {
		if len(t.Retry) > 0 {
			return batch.Spec{}, fmt.Errorf("task %s: retry does not belong in an eval", t.ID)
		}
	}
	if spec.Judge != nil {
		if err := spec.Judge.check(spec.Tasks); err != nil {
			return batch.Spec{}, err
		}
		if spec.Worktrees == batch.WorktreesSlots {
			return batch.Spec{}, fmt.Errorf("worktrees %q does not go with a judge, which reads each cell's worktree after the batch, when its slot may hold another task; leave worktrees out or use %q",
				spec.Worktrees, batch.WorktreesFresh)
		}
	}

	seen := map[string]bool{}
	for i, v := range spec.Matrix {
		name := v.name()
		if name == "" {
			return batch.Spec{}, fmt.Errorf("variant %d has no name and no backend", i+1)
		}
		if err := checkPart("variant", name); err != nil {
			return batch.Spec{}, err
		}
		if seen[name] {
			return batch.Spec{}, fmt.Errorf("duplicate variant %q: give one of them a name", name)
		}
		seen[name] = true
		if v.Backend != "" {
			if _, err := agent.Get(v.Backend); err != nil {
				return batch.Spec{}, fmt.Errorf("variant %s: %w", name, err)
			}
		}
	}

	bs := spec.Spec // the batch fields carry through unchanged
	// Shaping is the measured variant's choice, including its false value.
	bs.ShapeTools = false
	if bs.Worktrees == "" {
		// Each cell keeps its own worktree until the eval is done with it.
		bs.Worktrees = batch.WorktreesFresh
	}
	bs.Retry = nil
	bs.NoSwitch = true
	bs.Tasks = nil
	for _, t := range spec.Tasks {
		if t.ID == "" {
			return batch.Spec{}, fmt.Errorf("every eval task needs an id, so its results can be compared")
		}
		if err := checkPart("task id", t.ID); err != nil {
			return batch.Spec{}, err
		}
		for _, v := range spec.Matrix {
			for r := 1; r <= spec.Repeats; r++ {
				cell := t
				cell.ShapeTools = v.ShapeTools
				cell.ID = fmt.Sprintf("%s.%s.%s.r%d", evalID, t.ID, v.name(), r)
				// Pin the measured backend so purpose routing cannot change a cell.
				cell.Backend = firstOf(v.Backend, t.Backend, spec.Backend, agent.DefaultBackendNameIn(runlog.Home()))
				if v.Lean != nil {
					if cell.Backend != "claude" {
						return batch.Spec{}, fmt.Errorf("variant %s: lean is only supported by the claude backend (task %s uses %s)", v.name(), t.ID, cell.Backend)
					}
					cell.Lean = v.Lean
				}
				cell.Model = firstOf(v.Model, t.Model)
				cell.Effort = firstOf(v.Effort, t.Effort)
				cell.Retrieval = firstOf(v.Retrieval, t.Retrieval)
				cell.RetrievalVersion = firstOf(v.RetrievalVersion, t.RetrievalVersion)
				cell.Retry = nil
				bs.Tasks = append(bs.Tasks, cell)
			}
		}
	}
	return bs, nil
}

// checkPart rejects anything that would break the cell id, which is read back
// by splitting on dots: <evalid>.<task>.<variant>.r<n>.
func checkPart(kind, value string) error {
	if !partRE.MatchString(value) {
		return fmt.Errorf("%s %q: use letters, digits, '_' and '-' (no dots: they separate the parts of a cell id)", kind, value)
	}
	return nil
}

var partRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// VariantRow is what one variant did across every task and repeat.
type VariantRow struct {
	Name     string `json:"name"`
	Attempts int    `json:"attempts"`
	Passed   int    `json:"passed"`
	// Unchanged counts the passes that committed nothing: right for "find
	// the bug, if any", suspicious for "fix the typos".
	Unchanged      int            `json:"passed_unchanged,omitempty"`
	Statuses       map[string]int `json:"statuses"` // run status histogram
	MedianCostUSD  float64        `json:"median_cost_usd"`
	TotalCostUSD   float64        `json:"total_cost_usd"`
	CostEstimate   bool           `json:"cost_is_estimate,omitempty"`
	MedianDuration time.Duration  `json:"median_duration_ns"`
	MedianTurns    float64        `json:"median_turns"`
	MedianTools    float64        `json:"median_tool_calls"`
	// MedianContext counts every input token once, cached or not: what the
	// model was given, not what was billed at full rate.
	MedianContext float64 `json:"median_context_tokens"`
	// Judged counts the cells the judge scored; MeanScore is their mean, 0-10.
	Judged    int     `json:"judged,omitempty"`
	MeanScore float64 `json:"mean_score,omitempty"`
}

// PassRate is the share of cells that passed, as a share of attempts.
func (v VariantRow) PassRate() float64 {
	if v.Attempts == 0 {
		return 0
	}
	return float64(v.Passed) / float64(v.Attempts)
}

// TaskRow is one task's score under one variant, over the repeats.
type TaskRow struct {
	Task    string `json:"task"`
	Variant string `json:"variant"`
	Passed  int    `json:"passed"`
	Total   int    `json:"total"`
	// Judged and MeanScore are as in VariantRow, for this task alone.
	Judged    int     `json:"judged,omitempty"`
	MeanScore float64 `json:"mean_score,omitempty"`
}

// Score reads as "2/3", or "2/3 7.5" with the judge's mean score.
func (r TaskRow) Score() string {
	s := fmt.Sprintf("%d/%d", r.Passed, r.Total)
	if r.Judged > 0 {
		s += fmt.Sprintf(" %.1f", r.MeanScore)
	}
	return s
}

type Table struct {
	Variants []VariantRow `json:"variants"`
	Tasks    []TaskRow    `json:"tasks"`
}

// Totals sums the whole eval: cells run, cells passed, and what it cost,
// judge included, with whether any of that cost is an estimate.
func (r Record) Totals() (cells, passed int, costUSD float64, estimate bool) {
	costUSD = r.JudgeCostUSD
	for _, v := range r.Table.Variants {
		cells += v.Attempts
		passed += v.Passed
		costUSD += v.TotalCostUSD
		estimate = estimate || v.CostEstimate
	}
	for _, j := range r.Judgements {
		estimate = estimate || j.CostEstimate
	}
	return cells, passed, costUSD, estimate
}

// Record is one eval, saved under $HRN_HOME/evals.
type Record struct {
	RetrievalManifest   *RetrievalManifest   `json:"retrieval_manifest,omitempty"`
	RetrievalComparison *RetrievalComparison `json:"retrieval_comparison,omitempty"`
	ID                  string               `json:"id"`
	BatchID             string               `json:"batch_id"`
	Started             time.Time            `json:"started"`
	Duration            time.Duration        `json:"duration_ns"`
	Spec                Spec                 `json:"spec"`
	Table               Table                `json:"table"`
	// Judgements are the judge's verdicts, one per passing cell.
	Judgements   []Judgement `json:"judgements,omitempty"`
	JudgeCostUSD float64     `json:"judge_cost_usd,omitempty"`
}

// Run expands the spec, runs it as a batch and scores the result. The batch
// result comes back too: the worktrees are still there, so the caller can look
// at them before removing them.
func Run(ctx context.Context, spec Spec, notify func(batch.Event)) (Record, batch.Result, error) {
	started := time.Now()
	id, err := newID()
	if err != nil {
		return Record{}, batch.Result{}, err
	}
	bs, err := Expand(spec, id)
	if err != nil {
		return Record{}, batch.Result{}, err
	}
	res, runErr := batch.Run(ctx, bs, notify)
	if runErr != nil && len(res.Tasks) == 0 {
		return Record{}, res, runErr
	}
	var verdicts []Judgement
	if spec.Judge != nil && ctx.Err() == nil {
		verdicts = judgeAll(ctx, spec, res, notify)
	}
	rec := Record{ID: id, BatchID: res.ID, Started: started, Spec: spec, Judgements: verdicts,
		Table: Summarize(spec, res, verdicts), Duration: time.Since(started)}
	for _, v := range verdicts {
		rec.JudgeCostUSD += v.CostUSD
	}
	if err := save(rec); err != nil {
		return rec, res, err
	}
	return rec, res, runErr
}

// Summarize scores a finished batch against the eval that produced it, with
// the judge's verdicts when there are any. It reads the ids the expansion
// wrote, so it works on a record loaded later.
func Summarize(spec Spec, res batch.Result, verdicts []Judgement) Table {
	type cell struct {
		task, variant string
		passed        bool
		tr            batch.TaskResult
		score         int
		judged        bool
	}
	scores := map[string]Judgement{}
	for _, v := range verdicts {
		if v.Scored() {
			scores[v.Cell] = v
		}
	}
	var cells []cell
	for _, tr := range res.Tasks {
		task, variant, ok := splitID(tr.ID)
		if !ok {
			continue
		}
		v, judged := scores[tr.ID]
		cells = append(cells, cell{task, variant, tr.Status == batch.StatusPass, tr, v.Score, judged})
	}

	// Variant order follows the matrix, so the table reads like the spec.
	var table Table
	for _, v := range spec.Matrix {
		name := v.name()
		row := VariantRow{Name: name, Statuses: map[string]int{}}
		var costs, durations, turns, calls, inputs []float64
		total := 0
		for _, c := range cells {
			if c.variant != name {
				continue
			}
			row.Attempts++
			if c.passed {
				row.Passed++
				if c.tr.Commit == "" {
					row.Unchanged++
				}
			}
			if c.judged {
				row.Judged++
				total += c.score
			}
			status := "no-attempt"
			if n := len(c.tr.Attempts); n > 0 {
				a := c.tr.Attempts[n-1]
				status = string(a.Result.Status)
				costs = append(costs, a.Result.CostUSD)
				turns = append(turns, float64(a.Result.Turns))
				calls = append(calls, float64(a.Result.ToolCalls))
				r := a.Result
				if r.Backend == "" {
					r.Backend = v.Backend
				}
				if total, known, _ := r.ModelTokens(); known {
					inputs = append(inputs, float64(total-r.OutputTokens))
				}
				row.TotalCostUSD += a.Result.CostUSD
				row.CostEstimate = row.CostEstimate || a.Result.CostEstimate
			}
			row.Statuses[status]++
			durations = append(durations, float64(c.tr.Duration))
		}
		row.MedianCostUSD = report.Median(costs)
		row.MedianDuration = time.Duration(report.Median(durations))
		row.MedianTurns = report.Median(turns)
		row.MedianTools = report.Median(calls)
		if row.Judged > 0 {
			row.MeanScore = float64(total) / float64(row.Judged)
		}
		row.MedianContext = report.Median(inputs)
		table.Variants = append(table.Variants, row)
	}

	// One row per task and variant, in spec order.
	for _, t := range spec.Tasks {
		for _, v := range spec.Matrix {
			name := v.name()
			row := TaskRow{Task: t.ID, Variant: name}
			total := 0
			for _, c := range cells {
				if c.task != t.ID || c.variant != name {
					continue
				}
				row.Total++
				if c.passed {
					row.Passed++
				}
				if c.judged {
					row.Judged++
					total += c.score
				}
			}
			if row.Judged > 0 {
				row.MeanScore = float64(total) / float64(row.Judged)
			}
			if row.Total > 0 {
				table.Tasks = append(table.Tasks, row)
			}
		}
	}
	return table
}

// splitID reads back what Expand wrote: <evalid>.<task>.<variant>.r<n>. The
// task id cannot contain a dot (batch ids allow one, so this is checked when
// the spec is expanded), but a variant name can, so the split works from both
// ends.
func splitID(id string) (task, variant string, ok bool) {
	i := strings.Index(id, ".")
	j := strings.LastIndex(id, ".r")
	if i < 0 || j <= i {
		return "", "", false
	}
	rest := id[i+1 : j]
	k := strings.Index(rest, ".")
	if k < 0 {
		return "", "", false
	}
	return rest[:k], rest[k+1:], true
}

func dir() string { return filepath.Join(runlog.Home(), "evals") }

func save(rec Record) error {
	if err := os.MkdirAll(dir(), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir(), rec.ID+".json"), data, 0o644)
}

// LoadRecord reads a saved eval.
func LoadRecord(id string) (Record, error) {
	var rec Record
	if id == "" || strings.ContainsAny(id, `/\`) {
		return rec, fmt.Errorf("invalid eval id %q", id)
	}
	data, err := os.ReadFile(filepath.Join(dir(), id+".json"))
	if err != nil {
		return rec, err
	}
	return rec, json.Unmarshal(data, &rec)
}

// List returns saved evals, newest first.
// ListAll returns every saved eval, newest first, and how many records could
// not be read. It reads them all: eval ids are random, so only the records
// themselves say which are newest.
func ListAll() (out []Record, unreadable int, err error) {
	entries, err := os.ReadDir(dir())
	if os.IsNotExist(err) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	for _, name := range names {
		rec, err := LoadRecord(name)
		if err != nil {
			unreadable++
			continue
		}
		out = append(out, rec)
	}
	// Eval ids are random, not timestamps, so the order comes from the records.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out, unreadable, nil
}

// newID is five hex characters: short enough to read in a branch name, wide
// enough that two evals in one repository do not collide. A draw that already
// names a saved eval is thrown back, because reusing one would overwrite its
// record and collide with its branches.
func newID() (string, error) {
	for try := 0; try < 10; try++ {
		b := make([]byte, 3)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		id := hex.EncodeToString(b)[:5]
		if _, err := os.Stat(filepath.Join(dir(), id+".json")); os.IsNotExist(err) {
			return id, nil
		}
	}
	return "", fmt.Errorf("could not find an unused eval id")
}
