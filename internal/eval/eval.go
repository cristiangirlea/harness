// Package eval is the scoring layer: one task set run across a matrix of
// backends, models and efforts, repeated, and judged by the same gates a batch
// uses. It answers "which backend, model and effort for this kind of task"
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
	// Judge is reserved for scoring a diff against a rubric with a model.
	// Nothing reads it yet; gates are the only scorer in this version.
	Judge json.RawMessage `json:"judge,omitempty"`
}

// Variant is one cell of the matrix: what to run the task set with.
type Variant struct {
	Name    string `json:"name,omitempty"` // default: backend[-model][-effort]
	Backend string `json:"backend,omitempty"`
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
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
// variant and repeat. Retry is deliberately nil, because an eval measures what
// a single attempt does; a retried attempt would score the harness, not the
// backend.
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

	seen := map[string]bool{}
	for i, v := range spec.Matrix {
		name := v.name()
		if name == "" {
			return batch.Spec{}, fmt.Errorf("variant %d has no name and no backend", i+1)
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
	bs.Retry = nil
	bs.Tasks = nil
	for _, t := range spec.Tasks {
		if t.ID == "" {
			return batch.Spec{}, fmt.Errorf("every eval task needs an id, so its results can be compared")
		}
		for _, v := range spec.Matrix {
			for r := 1; r <= spec.Repeats; r++ {
				cell := t
				cell.ID = fmt.Sprintf("%s.%s.%s.r%d", evalID, t.ID, v.name(), r)
				cell.Backend = firstOf(v.Backend, t.Backend)
				cell.Model = firstOf(v.Model, t.Model)
				cell.Effort = firstOf(v.Effort, t.Effort)
				cell.Retry = nil
				bs.Tasks = append(bs.Tasks, cell)
			}
		}
	}
	return bs, nil
}

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
	Name           string         `json:"name"`
	Attempts       int            `json:"attempts"`
	Passed         int            `json:"passed"`
	Statuses       map[string]int `json:"statuses"` // run status histogram
	MedianCostUSD  float64        `json:"median_cost_usd"`
	TotalCostUSD   float64        `json:"total_cost_usd"`
	CostEstimate   bool           `json:"cost_is_estimate,omitempty"`
	MedianDuration time.Duration  `json:"median_duration_ns"`
	MedianTurns    float64        `json:"median_turns"`
	MedianInput    float64        `json:"median_input_tokens"`
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
}

// Score reads as "2/3".
func (r TaskRow) Score() string { return fmt.Sprintf("%d/%d", r.Passed, r.Total) }

type Table struct {
	Variants []VariantRow `json:"variants"`
	Tasks    []TaskRow    `json:"tasks"`
}

// Record is one eval, saved under $HX_HOME/evals.
type Record struct {
	ID       string        `json:"id"`
	BatchID  string        `json:"batch_id"`
	Started  time.Time     `json:"started"`
	Duration time.Duration `json:"duration_ns"`
	Spec     Spec          `json:"spec"`
	Table    Table         `json:"table"`
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
	rec := Record{ID: id, BatchID: res.ID, Started: started, Spec: spec,
		Table: Summarize(spec, res), Duration: time.Since(started)}
	if err := save(rec); err != nil {
		return rec, res, err
	}
	return rec, res, runErr
}

// Summarize scores a finished batch against the eval that produced it. It
// reads the ids the expansion wrote, so it works on a record loaded later.
func Summarize(spec Spec, res batch.Result) Table {
	type cell struct {
		task, variant string
		passed        bool
		tr            batch.TaskResult
	}
	var cells []cell
	for _, tr := range res.Tasks {
		task, variant, ok := splitID(tr.ID)
		if !ok {
			continue
		}
		cells = append(cells, cell{task, variant, tr.Status == batch.StatusPass, tr})
	}

	// Variant order follows the matrix, so the table reads like the spec.
	var table Table
	for _, v := range spec.Matrix {
		name := v.name()
		row := VariantRow{Name: name, Statuses: map[string]int{}}
		var costs, durations, turns, inputs []float64
		for _, c := range cells {
			if c.variant != name {
				continue
			}
			row.Attempts++
			if c.passed {
				row.Passed++
			}
			status := "no-attempt"
			if n := len(c.tr.Attempts); n > 0 {
				a := c.tr.Attempts[n-1]
				status = string(a.Result.Status)
				costs = append(costs, a.Result.CostUSD)
				turns = append(turns, float64(a.Result.Turns))
				inputs = append(inputs, float64(a.Result.InputTokens+a.Result.CacheRead))
				row.TotalCostUSD += a.Result.CostUSD
				row.CostEstimate = row.CostEstimate || a.Result.CostEstimate
			}
			row.Statuses[status]++
			durations = append(durations, float64(c.tr.Duration))
		}
		row.MedianCostUSD = report.Median(costs)
		row.MedianDuration = time.Duration(report.Median(durations))
		row.MedianTurns = report.Median(turns)
		row.MedianInput = report.Median(inputs)
		table.Variants = append(table.Variants, row)
	}

	// One row per task and variant, in spec order.
	for _, t := range spec.Tasks {
		for _, v := range spec.Matrix {
			name := v.name()
			row := TaskRow{Task: t.ID, Variant: name}
			for _, c := range cells {
				if c.task != t.ID || c.variant != name {
					continue
				}
				row.Total++
				if c.passed {
					row.Passed++
				}
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
func List(limit int) ([]Record, error) {
	entries, err := os.ReadDir(dir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	var out []Record
	sort.Slice(names, func(i, j int) bool { return names[i] > names[j] })
	for _, name := range names {
		rec, err := LoadRecord(name)
		if err != nil {
			continue
		}
		out = append(out, rec)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

// newID is five hex characters: short enough to read in a branch name, wide
// enough that two evals in one repository do not collide.
func newID() (string, error) {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b)[:5], nil
}
