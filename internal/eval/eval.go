// Package eval scores repeated variants with batch gates and optional model judging.
package eval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/purpose"
	"github.com/cristiangirlea/harness/internal/report"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// Spec extends a batch with variants, repeats and optional judging.
type Spec struct {
	batch.Spec
	Matrix         []Variant `json:"matrix"`
	Repeats        int       `json:"repeats,omitempty"` // default 1
	RerunUnstarted int       `json:"rerun_unstarted,omitempty"`
	// Judge optionally grades passing diffs against a rubric.
	Judge *Judge `json:"judge,omitempty"`
}

// UnmarshalJSON distinguishes an omitted rerun policy from an explicit zero.
func (s *Spec) UnmarshalJSON(data []byte) error {
	type plain Spec
	v := plain{RerunUnstarted: 1}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*s = Spec(v)
	return nil
}

// MarshalJSON preserves disabled reruns when a saved spec is loaded again.
func (s Spec) MarshalJSON() ([]byte, error) {
	type plain Spec
	return json.Marshal(struct {
		plain
		RerunUnstarted *int `json:"rerun_unstarted,omitempty"`
	}{plain(s), &s.RerunUnstarted})
}

// Variant selects a matrix cell's backend and harness.
type Variant struct {
	Lean             *bool  `json:"lean,omitempty"`
	ShapeTools       bool   `json:"shape_tools,omitempty"`
	Retrieval        string `json:"retrieval,omitempty"`
	RetrievalVersion string `json:"retrieval_version,omitempty"`
	Name             string `json:"name,omitempty"` // default: backend[-model][-effort]
	Backend          string `json:"backend,omitempty"`
	Model            string `json:"model,omitempty"`
	Effort           string `json:"effort,omitempty"`
	// PromptPrefix changes the brief without changing the judged task.
	PromptPrefix       string `json:"prompt_prefix,omitempty"`
	AppendSystemPrompt string `json:"append_system_prompt,omitempty"`
	NoSkills           bool   `json:"no_skills,omitempty"`
}

// ChangesHarness reports changes beyond model selection.
func (v Variant) ChangesHarness() bool {
	return v.PromptPrefix != "" || v.AppendSystemPrompt != "" || v.NoSkills
}

// Key returns Name or backend[-model][-effort].
func (v Variant) Key() string {
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

// LoadSpec resolves relative dirs from the spec's directory.
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

// Expand creates one batch task per task, variant and repeat.
// No retries or switching: each cell measures one attempt on its chosen backend.
func Expand(spec Spec, evalID string) (batch.Spec, error) {
	if len(spec.Matrix) == 0 {
		return batch.Spec{}, fmt.Errorf("matrix is empty: an eval needs at least one variant to compare")
	}
	if spec.Repeats < 0 {
		return batch.Spec{}, fmt.Errorf("repeats %d is negative", spec.Repeats)
	}
	if spec.RerunUnstarted < 0 {
		return batch.Spec{}, fmt.Errorf("rerun_unstarted %d is negative", spec.RerunUnstarted)
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
		name := v.Key()
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

	bs := spec.Spec
	// Each variant must control shaping, including false.
	bs.ShapeTools = false
	if bs.Worktrees == "" {
		// Preserve each cell's worktree for judging and inspection.
		bs.Worktrees = batch.WorktreesFresh
	}
	bs.Retry = nil
	bs.NoSwitch = true
	if bs.ProtectMode == "" {
		// A cell that weakened its own gates scored nothing real.
		bs.ProtectMode = batch.ProtectFail
	}
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
				cell.ID = fmt.Sprintf("%s.%s.%s.r%d", evalID, t.ID, v.Key(), r)
				// Routing must not change the measured backend.
				cell.Backend = firstOf(v.Backend, t.Backend, spec.Backend, agent.DefaultBackendNameIn(runlog.Home()))
				if v.Lean != nil {
					if cell.Backend != "claude" && cell.Backend != "codex" {
						return batch.Spec{}, fmt.Errorf("variant %s: lean is only supported by the claude and codex backends (task %s uses %s)", v.Key(), t.ID, cell.Backend)
					}
					cell.Lean = v.Lean
				}
				if v.PromptPrefix != "" {
					// Prefixing would hide a leading Purpose: header.
					if cell.Purpose == "" {
						cell.Purpose = purpose.Declared(t.Prompt)
					}
					cell.Prompt = v.PromptPrefix + "\n\n" + t.Prompt
				}
				cell.AppendSystemPrompt = firstOf(v.AppendSystemPrompt, t.AppendSystemPrompt)
				cell.NoSkills = v.NoSkills || t.NoSkills
				if err := checkHarness(cell.Options.Over(spec.Options), cell.Backend); err != nil {
					return batch.Spec{}, fmt.Errorf("variant %s: %w (task %s uses %s)", v.Key(), err, t.ID, cell.Backend)
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

// Ignored options would score the same harness under different names.
func checkHarness(o agent.Options, backend string) error {
	if o.AppendSystemPrompt == "" && !o.NoSkills {
		return nil
	}
	b, err := agent.Get(backend)
	if err != nil {
		return err
	}
	if o.AppendSystemPrompt != "" && !agent.AppliesSystemPrompt(b) {
		return fmt.Errorf("append_system_prompt is only supported by the api and claude backends")
	}
	if o.NoSkills && !agent.AppliesNoSkills(b) {
		return fmt.Errorf("no_skills is only supported by the claude backend")
	}
	if o.NoSkills && o.IsLean() && (o.Skills == nil || !*o.Skills) {
		return fmt.Errorf(`no_skills changes nothing on a lean claude cell, which already runs without skills: set "skills": true (or "lean": false) on the spec or task`)
	}
	return nil
}

// Dots delimit the persisted cell id: <evalid>.<task>.<variant>.r<n>.
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

// VariantRow aggregates one variant's tasks and repeats.
type VariantRow struct {
	Name     string `json:"name"`
	Attempts int    `json:"attempts"`
	Passed   int    `json:"passed"`
	// DidNotAct excludes idle successes (no tools, second turn or change) from pass rates.
	DidNotAct        int     `json:"did_not_act,omitempty"`
	DidNotStart      int     `json:"did_not_start,omitempty"`
	Reruns           int     `json:"reruns,omitempty"`
	RerunsExhausted  int     `json:"reruns_exhausted,omitempty"`
	BackendStartRate float64 `json:"start_rate"`
	// AgentCommitted counts cells whose agent moved its branch or HEAD with git, which a prompt may forbid.
	AgentCommitted int `json:"agent_committed,omitempty"`
	// Unchanged counts passes without commits, valid for inspection tasks.
	Unchanged     int            `json:"passed_unchanged,omitempty"`
	Statuses      map[string]int `json:"statuses"` // run status histogram
	MedianCostUSD float64        `json:"median_cost_usd"`
	TotalCostUSD  float64        `json:"total_cost_usd"`
	CostEstimate  bool           `json:"cost_is_estimate,omitempty"`
	// CellsWithoutCost excludes unknowns from cost totals and medians.
	CellsWithoutCost int `json:"cells_without_cost,omitempty"`
	// CostCounted distinguishes counted costs from old rows' median fallback.
	CostCounted    bool          `json:"cost_counted,omitempty"`
	MedianDuration time.Duration `json:"median_duration_ns"`
	MedianTurns    float64       `json:"median_turns"`
	MedianTools    float64       `json:"median_tool_calls"`
	// MedianContext counts cached and uncached input once, regardless of billing.
	MedianContext float64 `json:"median_context_tokens"`
	// MedianCostPassedUSD and MedianContextPassed exclude cheap failures from efficiency comparisons.
	MedianCostPassedUSD float64 `json:"median_cost_usd_passed,omitempty"`
	PassedCostKnown     bool    `json:"passed_cost_known,omitempty"`
	MedianContextPassed float64 `json:"median_context_tokens_passed,omitempty"`
	// Judged counts scored cells; MeanScore averages verdicts, 0-10.
	Judged    int     `json:"judged,omitempty"`
	MeanScore float64 `json:"mean_score,omitempty"`
	// JudgeRepeats > 1 measures repeat-mean variation in ScoreMin/ScoreMax.
	JudgeRepeats int     `json:"judge_repeats,omitempty"`
	ScoreMin     float64 `json:"score_min,omitempty"`
	ScoreMax     float64 `json:"score_max,omitempty"`

	// Setup includes the first task's prompt_prefix; SetupMixed flags base-setup drift within task repeats.
	Setup      *agent.Setup `json:"setup,omitempty"`
	SetupTask  string       `json:"setup_task,omitempty"`
	SetupMixed bool         `json:"setup_mixed,omitempty"`
}

// UnmarshalJSON keeps old rows' start rates consistent with their recorded counts.
func (v *VariantRow) UnmarshalJSON(data []byte) error {
	type plain VariantRow
	var row struct {
		plain
		StartRate *float64 `json:"start_rate"`
	}
	if err := json.Unmarshal(data, &row); err != nil {
		return err
	}
	*v = VariantRow(row.plain)
	if row.StartRate != nil {
		v.BackendStartRate = *row.StartRate
	} else if v.Attempts > 0 {
		v.BackendStartRate = float64(v.Attempts-v.DidNotStart) / float64(v.Attempts)
	}
	return nil
}

type judgeScore struct {
	judged   int
	mean     float64
	repeats  int
	min, max float64
}

func (v VariantRow) judge() judgeScore {
	return judgeScore{v.Judged, v.MeanScore, v.JudgeRepeats, v.ScoreMin, v.ScoreMax}
}

func (j judgeScore) spread() (float64, bool) {
	return j.max - j.min, j.repeats > 1
}

func scoreOf(cells [][]Judgement) judgeScore {
	var out judgeScore
	total, count := 0, 0
	repeats := map[int]bool{}
	for _, cell := range cells {
		if len(cell) > 0 {
			out.judged++
		}
		for _, v := range cell {
			total += v.Score
			count++
			// Missing repeat numbers in saved records mean the first repeat.
			repeats[max(1, v.Repeat)] = true
		}
	}
	if count == 0 {
		return out
	}
	out.mean = float64(total) / float64(count)
	if len(repeats) < 2 {
		return out
	}
	// Use complete repeats so task difficulty cannot masquerade as judge noise.
	sums := map[int][2]int{}
	for _, cell := range cells {
		byRepeat := map[int]int{}
		for _, v := range cell {
			byRepeat[max(1, v.Repeat)] = v.Score
		}
		if len(byRepeat) != len(repeats) {
			continue
		}
		for r, score := range byRepeat {
			sums[r] = [2]int{sums[r][0] + score, sums[r][1] + 1}
		}
	}
	if len(sums) == 0 {
		return out
	}
	out.repeats = len(repeats)
	out.min, out.max = 10, 0
	for _, s := range sums {
		m := float64(s[0]) / float64(s[1])
		out.min, out.max = min(out.min, m), max(out.max, m)
	}
	return out
}

// CostKnown reports whether any cell of the variant has a known cost.
func (v VariantRow) CostKnown() bool {
	if !v.CostCounted {
		return v.MedianCostUSD != 0
	}
	return v.Attempts > v.CellsWithoutCost
}

// PassRate is the share of the cells that acted which passed.
func (v VariantRow) PassRate() float64 {
	acted := v.Attempts - v.DidNotAct - v.DidNotStart
	if acted <= 0 {
		return 0
	}
	return float64(v.Passed) / float64(acted)
}

func neverActed(tr batch.TaskResult) bool {
	if len(tr.Attempts) == 0 || tr.Commit != "" {
		return false
	}
	r := tr.Attempts[len(tr.Attempts)-1].Result
	return r.Status == agent.StatusOK && r.ToolCalls == 0 && r.Turns <= 1
}

// TaskRow aggregates a task's repeats under one variant.
type TaskRow struct {
	Task    string `json:"task"`
	Variant string `json:"variant"`
	Passed  int    `json:"passed"`
	Total   int    `json:"total"`
	// DidNotAct counts idle cells as in VariantRow.
	DidNotAct       int     `json:"did_not_act,omitempty"`
	DidNotStart     int     `json:"did_not_start,omitempty"`
	Reruns          int     `json:"reruns,omitempty"`
	RerunsExhausted int     `json:"reruns_exhausted,omitempty"`
	Judged          int     `json:"judged,omitempty"`
	MeanScore       float64 `json:"mean_score,omitempty"`
	JudgeRepeats    int     `json:"judge_repeats,omitempty"`
	ScoreMin        float64 `json:"score_min,omitempty"`
	ScoreMax        float64 `json:"score_max,omitempty"`
}

func (r TaskRow) judge() judgeScore {
	return judgeScore{r.Judged, r.MeanScore, r.JudgeRepeats, r.ScoreMin, r.ScoreMax}
}

// Score formats passes/acted cells, idle counts and judge mean/range.
func (r TaskRow) Score() string {
	s := fmt.Sprintf("%d/%d", r.Passed, r.Total-r.DidNotAct-r.DidNotStart)
	if r.DidNotAct > 0 {
		s += fmt.Sprintf(" +%d did not act", r.DidNotAct)
	}
	if r.DidNotStart > 0 {
		s += fmt.Sprintf(" +%d did not start", r.DidNotStart)
	}
	if r.Reruns > 0 {
		s += fmt.Sprintf(" +%d reruns", r.Reruns)
	}
	if r.RerunsExhausted > 0 {
		s += fmt.Sprintf(" (%d exhausted)", r.RerunsExhausted)
	}
	if r.Judged > 0 {
		s += fmt.Sprintf(" %.1f", r.MeanScore)
	}
	if r.Judged > 0 && r.JudgeRepeats > 1 {
		s += fmt.Sprintf(" (%.1f-%.1f)", r.ScoreMin, r.ScoreMax)
	}
	return s
}

type Table struct {
	Variants []VariantRow `json:"variants"`
	Tasks    []TaskRow    `json:"tasks"`
}

// Totals returns cell counts, costs including judging, and cost availability/estimate flags.
func (r Record) Totals() (cells, passed int, costUSD float64, known, estimate bool, withoutCost int) {
	costUSD = r.JudgeCostUSD
	known = r.JudgeCostUSD != 0
	for _, v := range r.Table.Variants {
		cells += v.Attempts
		passed += v.Passed
		costUSD += v.TotalCostUSD
		// Old zero medians can hide known costs; nonzero totals prove them.
		known = known || v.CostKnown() || v.TotalCostUSD != 0
		estimate = estimate || v.CostEstimate
		withoutCost += v.CellsWithoutCost
	}
	for _, j := range r.Judgements {
		estimate = estimate || j.CostEstimate
	}
	return cells, passed, costUSD, known, estimate, withoutCost
}

// Record saves an eval in $HRN_HOME/evals with its backing BatchID.
type Record struct {
	RetrievalManifest   *RetrievalManifest   `json:"retrieval_manifest,omitempty"`
	RetrievalComparison *RetrievalComparison `json:"retrieval_comparison,omitempty"`
	ID                  string               `json:"id"`
	BatchID             string               `json:"batch_id"`
	Started             time.Time            `json:"started"`
	Duration            time.Duration        `json:"duration_ns"`
	Spec                Spec                 `json:"spec"`
	Table               Table                `json:"table"`
	// Judgements holds a verdict per passing cell and judge repeat.
	Judgements   []Judgement        `json:"judgements,omitempty"`
	JudgeCostUSD float64            `json:"judge_cost_usd,omitempty"`
	Results      []batch.TaskResult `json:"results,omitempty"`
	// RerunBatchIDs are the batches of later rounds, each with its own worktrees and branches.
	RerunBatchIDs []string `json:"rerun_batch_ids,omitempty"`
	// SetupComparison belongs to the candidate's setup eval.
	SetupComparison *SetupComparison `json:"setup_comparison,omitempty"`
}

// Run scores the expanded batch, retaining worktrees for inspection.
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
	var rerunBatches []string
	for round := 1; round <= spec.RerunUnstarted && runErr == nil && ctx.Err() == nil; round++ {
		pending := map[string]bool{}
		for _, c := range groupCells(res.Tasks) {
			if neverStarted(c.scored) && neverStarted(c.results[len(c.results)-1]) {
				pending[c.first.ID] = true
			}
		}
		next := bs
		next.Base = res.Base
		next.Tasks = nil
		for _, task := range bs.Tasks {
			if pending[task.ID] {
				task.ID += fmt.Sprintf(".u%d", round)
				next.Tasks = append(next.Tasks, task)
			}
		}
		if len(next.Tasks) == 0 {
			break
		}
		rerun, err := batch.Run(ctx, next, notify)
		if rerun.ID != "" {
			rerunBatches = append(rerunBatches, rerun.ID)
		}
		res.Tasks = append(res.Tasks, rerun.Tasks...)
		res.Duration += rerun.Duration
		runErr = errors.Join(runErr, err)
	}
	var verdicts []Judgement
	if spec.Judge != nil && ctx.Err() == nil {
		scored := res
		scored.Tasks = nil
		for _, c := range groupCells(res.Tasks) {
			scored.Tasks = append(scored.Tasks, c.scored)
		}
		verdicts = judgeAll(ctx, spec, scored, notify)
	}
	rec := Record{ID: id, BatchID: res.ID, Started: started, Spec: spec, Judgements: verdicts,
		Table: Summarize(spec, res, verdicts), Results: res.Tasks, RerunBatchIDs: rerunBatches, Duration: time.Since(started)}
	for _, v := range verdicts {
		rec.JudgeCostUSD += v.CostUSD
	}
	if err := save(rec); err != nil {
		return rec, res, err
	}
	return rec, res, runErr
}

// neverStarted is true only when no attempt reached the task: one that ran and failed is a real failure.
func neverStarted(tr batch.TaskResult) bool {
	for _, a := range tr.Attempts {
		if a.DidNotStart == "" {
			return false
		}
	}
	return len(tr.Attempts) > 0
}

type cellResults struct {
	first, scored batch.TaskResult
	results       []batch.TaskResult
}

func groupCells(results []batch.TaskResult) []cellResults {
	var cells []cellResults
	byID := map[string]int{}
	for _, tr := range results {
		id := tr.ID
		if i := strings.LastIndex(id, ".u"); i >= 0 {
			if round, err := strconv.Atoi(id[i+2:]); err == nil && round > 0 {
				id = id[:i]
			}
		}
		if i, ok := byID[id]; ok {
			c := &cells[i]
			c.results = append(c.results, tr)
			if neverStarted(c.scored) && len(tr.Attempts) > 0 && !neverStarted(tr) {
				c.scored = tr
			}
		} else if id == tr.ID {
			byID[id] = len(cells)
			cells = append(cells, cellResults{first: tr, scored: tr, results: []batch.TaskResult{tr}})
		}
	}
	return cells
}

// Summarize scores a batch and judge verdicts by persisted cell ids.
func Summarize(spec Spec, res batch.Result, verdicts []Judgement) Table {
	type cell struct {
		task, variant    string
		passed           bool
		tr               batch.TaskResult
		verdicts         []Judgement
		idle             bool
		unstarted        bool
		firstUnstarted   bool
		rerun, exhausted bool
		results          []batch.TaskResult
	}
	answerOnly := map[string]bool{}
	for _, t := range spec.Tasks {
		answerOnly[t.ID] = t.Expect != "" && len(firstOfList(t.Gate, spec.Gate)) == 0
	}
	scores := map[string][]Judgement{}
	for _, v := range verdicts {
		if v.Scored() {
			scores[v.Cell] = append(scores[v.Cell], v)
		}
	}
	var cells []cell
	for _, group := range groupCells(res.Tasks) {
		tr := group.scored
		task, variant, ok := splitID(tr.ID)
		if !ok {
			continue
		}
		unstarted := neverStarted(tr)
		passed := tr.Status == batch.StatusPass && !unstarted
		rerun := len(group.results) > 1
		cells = append(cells, cell{task: task, variant: variant, passed: passed, tr: tr, verdicts: scores[tr.ID],
			idle: !unstarted && !passed && !answerOnly[task] && neverActed(tr), unstarted: unstarted,
			firstUnstarted: neverStarted(group.first), rerun: rerun,
			exhausted: rerun && unstarted && len(group.results)-1 >= spec.RerunUnstarted, results: group.results})
	}

	var table Table
	for _, v := range spec.Matrix {
		name := v.Key()
		row := VariantRow{Name: name, Statuses: map[string]int{}}
		var costs, durations, turns, calls, inputs, passedCosts, passedInputs []float64
		var judged [][]Judgement
		taskSetups := map[string]string{}
		for _, c := range cells {
			if c.variant != name {
				continue
			}
			row.Attempts++
			if !c.firstUnstarted {
				row.BackendStartRate++
			}
			if c.rerun {
				row.Reruns++
			}
			if c.exhausted {
				row.RerunsExhausted++
			}
			for _, tr := range c.results {
				for _, a := range tr.Attempts {
					if cost, known, estimated := a.Result.KnownCost(); known {
						row.TotalCostUSD += cost
						row.CostEstimate = row.CostEstimate || estimated
					}
				}
			}
			if c.tr.AgentCommitted() {
				row.AgentCommitted++
			}
			if c.idle || c.unstarted {
				// Setup failures cannot measure task performance.
				if c.unstarted {
					row.DidNotStart++
				} else {
					row.DidNotAct++
				}
				row.Statuses[string(c.tr.Attempts[len(c.tr.Attempts)-1].Result.Status)]++
				continue
			}
			if c.passed {
				row.Passed++
				if c.tr.Commit == "" {
					row.Unchanged++
				}
			}
			judged = append(judged, c.verdicts)
			status := "no-attempt"
			costKnown := false
			if n := len(c.tr.Attempts); n > 0 {
				a := c.tr.Attempts[n-1]
				status = string(a.Result.Status)
				if s := runSetup(a.RunID); s != nil {
					// Compare the same first task on both sides of a diff.
					if row.Setup == nil && len(spec.Tasks) > 0 && c.task == spec.Tasks[0].ID {
						row.Setup, row.SetupTask = s, c.task
						if v.PromptPrefix != "" {
							row.Setup = s.WithOptions(agent.PromptPrefixOption(v.PromptPrefix))
						}
					}
					// Task-specific MCP configs may differ; repeats must agree.
					if was, ok := taskSetups[c.task]; !ok {
						taskSetups[c.task] = s.BaseDigest
					} else if was != s.BaseDigest {
						row.SetupMixed = true
					}
				}
				if cost, known, estimated := a.Result.KnownCost(); known {
					costKnown = true
					costs = append(costs, cost)
					if c.passed {
						passedCosts = append(passedCosts, cost)
					}
					row.CostEstimate = row.CostEstimate || estimated
				}
				turns = append(turns, float64(a.Result.Turns))
				calls = append(calls, float64(a.Result.ToolCalls))
				r := a.Result
				if r.Backend == "" {
					r.Backend = v.Backend
				}
				if total, known, _ := r.ModelTokens(); known {
					inputs = append(inputs, float64(total-r.OutputTokens))
					if c.passed {
						passedInputs = append(passedInputs, float64(total-r.OutputTokens))
					}
				}
			}
			if !costKnown {
				row.CellsWithoutCost++
			}
			row.Statuses[status]++
			durations = append(durations, float64(c.tr.Duration))
		}
		row.MedianCostUSD = report.Median(costs)
		if row.Attempts > 0 {
			row.BackendStartRate /= float64(row.Attempts)
		}
		row.CostCounted = true
		row.MedianDuration = time.Duration(report.Median(durations))
		row.MedianTurns = report.Median(turns)
		row.MedianTools = report.Median(calls)
		js := scoreOf(judged)
		row.Judged, row.MeanScore, row.JudgeRepeats, row.ScoreMin, row.ScoreMax = js.judged, js.mean, js.repeats, js.min, js.max
		row.MedianContext = report.Median(inputs)
		row.MedianContextPassed = report.Median(passedInputs)
		if len(passedCosts) > 0 {
			row.MedianCostPassedUSD, row.PassedCostKnown = report.Median(passedCosts), true
		}
		table.Variants = append(table.Variants, row)
	}

	for _, t := range spec.Tasks {
		for _, v := range spec.Matrix {
			name := v.Key()
			row := TaskRow{Task: t.ID, Variant: name}
			var judged [][]Judgement
			for _, c := range cells {
				if c.task != t.ID || c.variant != name {
					continue
				}
				row.Total++
				if c.rerun {
					row.Reruns++
				}
				if c.exhausted {
					row.RerunsExhausted++
				}
				if c.unstarted {
					row.DidNotStart++
					continue
				}
				if c.idle {
					row.DidNotAct++
				}
				if c.passed {
					row.Passed++
				}
				judged = append(judged, c.verdicts)
			}
			js := scoreOf(judged)
			row.Judged, row.MeanScore, row.JudgeRepeats, row.ScoreMin, row.ScoreMax = js.judged, js.mean, js.repeats, js.min, js.max
			if row.Total > 0 {
				table.Tasks = append(table.Tasks, row)
			}
		}
	}
	return table
}

func runSetup(id string) *agent.Setup {
	if id == "" {
		return nil
	}
	rec, err := runlog.Load(id)
	if err != nil {
		return nil
	}
	return rec.Setup
}

// Split persisted <evalid>.<task>.<variant>.r<n> from both ends for dotted variants.
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
	if err := os.MkdirAll(dir(), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir(), rec.ID+".json"), data, 0o600)
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

// ListAll returns saved evals newest first and counts unreadable records.
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
	// Random ids cannot establish record order.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out, unreadable, nil
}

// Reject saved ids to prevent overwriting records and colliding with their branches.
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
