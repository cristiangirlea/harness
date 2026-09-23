package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/tools"
)

// Judge scores what each passing cell did against a rubric, with a model.
// Gates prove the build and expect proves the answer; neither can tell a
// typo fix from a spelling change. The judge reads the diff and says.
type Judge struct {
	Backend string `json:"backend,omitempty"` // default: the default backend
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
	// Rubric applies to every task; Rubrics replaces it for the tasks it
	// names. Every task needs one or the other.
	Rubric  string            `json:"rubric,omitempty"`
	Rubrics map[string]string `json:"rubrics,omitempty"`
	// Timeout is per judgement; default 5m.
	Timeout agent.Duration `json:"timeout,omitempty"`
}

// Judgement is the judge's verdict on one cell.
type Judgement struct {
	Cell         string  `json:"cell"`
	Score        int     `json:"score"` // 0-10; meaningless when Error is set
	Reason       string  `json:"reason,omitempty"`
	RunID        string  `json:"run_id,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
	CostEstimate bool    `json:"cost_is_estimate,omitempty"`
	Error        string  `json:"error,omitempty"` // the judge could not score this cell
}

// Scored reports whether the judgement carries a score.
func (j Judgement) Scored() bool { return j.Error == "" }

const (
	defaultJudgeTimeout = 5 * time.Minute
	maxJudgeDiff        = 60_000 // bytes of diff shown to the judge
	maxJudgeAnswer      = 8_000
)

func (j *Judge) rubricFor(task string) string {
	if r, ok := j.Rubrics[task]; ok {
		return r
	}
	return j.Rubric
}

// check rejects a judge that could only fail once the agents have run.
func (j *Judge) check(tasks []batch.Task) error {
	if j.Backend != "" {
		if _, err := agent.Get(j.Backend); err != nil {
			return fmt.Errorf("judge: %w", err)
		}
	}
	if j.Timeout < 0 {
		return fmt.Errorf("judge: timeout %s is negative", time.Duration(j.Timeout))
	}
	ids := map[string]bool{}
	for _, t := range tasks {
		ids[t.ID] = true
		if strings.TrimSpace(j.rubricFor(t.ID)) == "" {
			return fmt.Errorf("judge: task %s has no rubric; set judge.rubric or judge.rubrics[%q]", t.ID, t.ID)
		}
	}
	for id := range j.Rubrics {
		if !ids[id] {
			return fmt.Errorf("judge: rubrics names %q, which is not a task", id)
		}
	}
	return nil
}

// judgeAll scores every passing cell, spec.Parallel at a time. A cell that
// failed its gates is not judged: it already scored zero where it counts, and
// judging it would only spend money. The worktrees must still exist.
func judgeAll(ctx context.Context, spec Spec, res batch.Result, notify func(batch.Event)) []Judgement {
	j := spec.Judge
	// Verdicts arrive from several goroutines; callers expect one at a time,
	// as batch.Run delivers its own events.
	var notifyMu sync.Mutex
	report := notify
	notify = func(e batch.Event) {
		if report == nil {
			return
		}
		notifyMu.Lock()
		defer notifyMu.Unlock()
		report(e)
	}
	b, err := agent.Get(firstOf(j.Backend, agent.DefaultBackendName()))
	if err != nil {
		notify(batch.Event{Kind: "judge", Text: "judge: " + err.Error()})
		return nil
	}
	prompts := map[string]string{}
	for _, t := range spec.Tasks {
		prompts[t.ID] = t.Prompt
	}

	var cells []batch.TaskResult
	for _, tr := range res.Tasks {
		if _, _, ok := splitID(tr.ID); ok && tr.Status == batch.StatusPass {
			cells = append(cells, tr)
		}
	}
	out := make([]Judgement, len(cells))
	// The spec as loaded, not as batch.Run normalised it: an unset parallel
	// is 0 here, and must mean the batch default rather than one at a time.
	parallel := spec.Parallel
	if parallel <= 0 {
		parallel = batch.DefaultParallel
	}
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, tr := range cells {
		if ctx.Err() != nil {
			out[i] = Judgement{Cell: tr.ID, Error: "canceled"}
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			task, _, _ := splitID(tr.ID)
			out[i] = judgeCell(ctx, b, j, res, tr, prompts[task], j.rubricFor(task))
			text := fmt.Sprintf("judge: %d/10 · %s", out[i].Score, tools.OneLine(out[i].Reason, 120))
			if !out[i].Scored() {
				text = "judge: " + tools.OneLine(out[i].Error, 160)
			}
			notify(batch.Event{Task: tr.ID, Kind: "judge", Text: text})
		}()
	}
	wg.Wait()
	return out
}

func judgeCell(ctx context.Context, b agent.Backend, j *Judge, res batch.Result, tr batch.TaskResult, prompt, rubric string) Judgement {
	jd := Judgement{Cell: tr.ID}
	diff, untracked, err := batch.Diff(ctx, res, tr.ID)
	if err != nil {
		jd.Error = "reading the diff: " + err.Error()
		return jd
	}
	var final string
	if n := len(tr.Attempts); n > 0 {
		final = tr.Attempts[n-1].Result.Final
	}

	timeout := time.Duration(j.Timeout)
	if timeout == 0 {
		timeout = defaultJudgeTimeout
	}
	task := agent.Task{
		Prompt: judgePrompt(prompt, rubric, final, diff, untracked),
		// The worktree, read-only: the judge may open a file the diff only
		// touches, but it cannot change the evidence.
		Dir: tr.Worktree,
		Options: agent.Options{Mode: agent.ModeReadOnly, Model: j.Model, Effort: j.Effort,
			Timeout: agent.Duration(timeout), NoInstructions: true},
	}
	rec, err := runlog.Create(b.Name(), task)
	if err != nil {
		jd.Error = "creating run record: " + err.Error()
		return jd
	}
	jd.RunID = rec.ID()
	r := agent.Run(ctx, b, task, rec.Event)
	_ = rec.Close(r)
	jd.CostUSD, jd.CostEstimate = r.CostUSD, r.CostEstimate
	if r.Status != agent.StatusOK {
		jd.Error = fmt.Sprintf("judge run %s: %s %s", jd.RunID, r.Status, r.Error)
		return jd
	}
	score, reason, err := parseVerdict(r.Final)
	if err != nil {
		jd.Error = fmt.Sprintf("judge run %s: %v", jd.RunID, err)
		return jd
	}
	jd.Score, jd.Reason = score, reason
	return jd
}

const judgeInstructions = `You are grading one attempt by a coding agent. Score how well the change does
what the task asked, measured against the rubric. Judge the change itself: an
explanation that claims more than the diff shows earns nothing. An empty diff
is right only when the task needed no change.

Scores: 10 does exactly what was asked and nothing else; 7 does it with minor
flaws; 4 partly does it or adds unrequested changes; 0 does the wrong thing or
nothing.

Reply with one line of JSON as the last thing you write, and nothing after it:
{"score": <integer 0-10>, "reason": "<one sentence>"}
`

func judgePrompt(prompt, rubric, final, diff string, untracked []string) string {
	var b strings.Builder
	b.WriteString(judgeInstructions)
	section := func(name, body string) {
		f := fence(body)
		fmt.Fprintf(&b, "\n## %s\n%s\n%s\n%s\n", name, f, body, f)
	}
	section("Task", prompt)
	section("Rubric", rubric)
	section("Agent's final answer", clip(final, maxJudgeAnswer))
	if strings.TrimSpace(diff) == "" {
		diff = "(no changes)"
	}
	section("Diff", clip(diff, maxJudgeDiff))
	if len(untracked) > 0 {
		section("Untracked files (not in the diff)", strings.Join(untracked, "\n"))
	}
	return b.String()
}

// clip keeps the start of s and says how much was cut.
func clip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n[... %d more bytes not shown]", len(s)-cut)
}

// fence returns a code fence longer than any run of backticks in s.
func fence(s string) string {
	f := "```"
	for strings.Contains(s, f) {
		f += "`"
	}
	return f
}

// parseVerdict reads the last JSON object in the judge's answer that has a
// score. Models wrap JSON in prose or fences, so it scans from the end.
func parseVerdict(text string) (int, string, error) {
	for i := strings.LastIndex(text, "{"); i >= 0; i = strings.LastIndex(text[:i], "{") {
		var v struct {
			Score  *float64 `json:"score"`
			Reason string   `json:"reason"`
		}
		if err := json.NewDecoder(strings.NewReader(text[i:])).Decode(&v); err != nil || v.Score == nil {
			continue
		}
		s := *v.Score
		if s < 0 || s > 10 || s != float64(int(s)) {
			return 0, "", fmt.Errorf("score %v is not a whole number from 0 to 10", s)
		}
		return int(s), strings.TrimSpace(v.Reason), nil
	}
	return 0, "", errors.New(`the answer has no {"score": ...} object`)
}
