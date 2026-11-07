package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cristiangirlea/harness/internal/followup"
	"github.com/cristiangirlea/harness/internal/remote"
	"github.com/cristiangirlea/harness/internal/resolve"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/tools"
)

// Judge grades passing cells against a rubric beyond deterministic checks.
type Judge struct {
	Backend string `json:"backend,omitempty"` // default: the default backend
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
	// Rubric is required unless Rubrics supplies a task-specific replacement.
	Rubric  string            `json:"rubric,omitempty"`
	Rubrics map[string]string `json:"rubrics,omitempty"`
	// Timeout is per judgement; default 5m.
	Timeout agent.Duration `json:"timeout,omitempty"`
	// Repeats defaults to 1; repeats measure judge noise.
	Repeats  int         `json:"repeats,omitempty"`
	Criteria []Criterion `json:"criteria,omitempty"`
	NotFlag  []string    `json:"not_flag,omitempty"`
}

type Criterion struct {
	ID    string `json:"id"`
	Check string `json:"check"`
}

type CriterionVerdict struct {
	ID         string `json:"id"`
	Met        bool   `json:"met"`
	Evidence   string `json:"evidence"`
	Downgraded bool   `json:"downgraded,omitempty"`
}

// Judgement is the judge's verdict on one cell.
type Judgement struct {
	Cell         string             `json:"cell"`
	Score        int                `json:"score"` // 0-10; meaningless when Error is set
	Reason       string             `json:"reason,omitempty"`
	RunID        string             `json:"run_id,omitempty"`
	CostUSD      float64            `json:"cost_usd,omitempty"`
	CostEstimate bool               `json:"cost_is_estimate,omitempty"`
	Error        string             `json:"error,omitempty"`  // the judge could not score this cell
	Repeat       int                `json:"repeat,omitempty"` // 1-based when the judge repeats; unset otherwise
	Criteria     []CriterionVerdict `json:"criteria,omitempty"`
}

// Scored reports whether the judgement carries a score.
func (j Judgement) Scored() bool { return j.Error == "" }

const (
	defaultJudgeTimeout = 5 * time.Minute
	maxJudgeDiff        = 60_000 // bytes of diff shown to the judge
	maxJudgeAnswer      = 8_000
	maxEvidence         = 500 // bytes of a criterion's evidence stored and scanned for references
)

var criterionID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
var diffHeaderPaths = regexp.MustCompile(`^diff --git ("(?:\\.|[^"])*"|a/.*?) ("(?:\\.|[^"])*"|b/.*)$`)
var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

func (j *Judge) rubricFor(task string) string {
	if r, ok := j.Rubrics[task]; ok {
		return r
	}
	return j.Rubric
}

func (j *Judge) check(tasks []batch.Task) error {
	p, err := resolve.BackendPreferences(runlog.Home())
	if err != nil {
		return fmt.Errorf("judge: %w", err)
	}
	if err := p.Check(firstOf(j.Backend, resolve.DefaultBackendNameIn(runlog.Home()))); err != nil {
		return fmt.Errorf("judge: %w", err)
	}
	if j.Backend != "" {
		if _, err := agent.Get(j.Backend); err != nil {
			return fmt.Errorf("judge: %w", err)
		}
	}
	if j.Timeout < 0 {
		return fmt.Errorf("judge: timeout %s is negative", time.Duration(j.Timeout))
	}
	if j.Repeats < 0 {
		return fmt.Errorf("judge: repeats %d is negative", j.Repeats)
	}
	if len(j.Criteria) > 30 {
		return errors.New("judge: at most 30 criteria are allowed")
	}
	if len(j.NotFlag) > 0 && len(j.Criteria) == 0 {
		return errors.New("judge: not_flag needs criteria")
	}
	seen := map[string]bool{}
	for _, c := range j.Criteria {
		if !criterionID.MatchString(c.ID) {
			return fmt.Errorf("judge: invalid criterion id %q; use letters, digits, underscores or hyphens", c.ID)
		}
		if seen[c.ID] {
			return fmt.Errorf("judge: duplicate criterion id %q", c.ID)
		}
		seen[c.ID] = true
		if strings.TrimSpace(c.Check) == "" {
			return fmt.Errorf("judge: criterion %q has an empty check", c.ID)
		}
	}
	// Validate effort before spending on cells the judge cannot grade.
	if err := agent.ValidateOptions(agent.Options{Effort: j.Effort}); err != nil {
		return fmt.Errorf("judge: %w", err)
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

// Failed gates need no paid judging; passing worktrees must remain available.
func judgeAll(ctx context.Context, spec Spec, res batch.Result, notify func(batch.Event)) []Judgement {
	j := spec.Judge
	// Serialize callbacks to preserve batch.Run's notification contract.
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
	b, err := followup.Backend(firstOf(j.Backend, resolve.DefaultBackendNameIn(runlog.Home())))
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
	// Lazy copies give repeats identical evidence without accumulating unused trees.
	dirs := make([]string, len(cells)) // each written once, by its cell's first verdict
	shown := make([]func() evidence, len(cells))
	for i, tr := range cells {
		task, _, _ := splitID(tr.ID)
		shown[i] = sync.OnceValue(func() evidence {
			ev := evidenceOf(ctx, res, tr, prompts[task], j.rubricFor(task), j)
			dirs[i] = ev.dir
			return ev
		})
	}
	repeats := max(1, j.Repeats)
	out := make([]Judgement, len(cells)*repeats)
	// The unnormalized spec's zero still means batch.DefaultParallel.
	parallel := spec.Parallel
	if parallel <= 0 {
		parallel = batch.DefaultParallel
	}
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	// Remove after the final repeat to bound checkout storage.
	left := make([]atomic.Int32, len(cells))
	for i := range left {
		left[i].Store(int32(repeats))
	}
	// Cancellation can leave copies whose final repeat never ran.
	defer func() {
		for i, dir := range dirs {
			if dir == "" || left[i].Load() == 0 {
				continue
			}
			if err := os.RemoveAll(dir); err != nil {
				notify(batch.Event{Task: cells[i].ID, Kind: "judge", Text: "judge: leaving its checkout behind: " + err.Error()})
			}
		}
	}()
	for _, n := range judgeOrder(len(out), res.ID) {
		tr, evidenceFor := cells[n/repeats], shown[n/repeats]
		repeat, label := 0, "judge"
		if repeats > 1 {
			repeat = n%repeats + 1
			label = fmt.Sprintf("judge %d/%d", repeat, repeats)
		}
		if ctx.Err() != nil {
			out[n] = Judgement{Cell: tr.ID, Error: "canceled", Repeat: repeat}
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			// A cancel may land while this verdict waited for its turn.
			if ctx.Err() != nil {
				out[n] = Judgement{Cell: tr.ID, Error: "canceled", Repeat: repeat}
				return
			}
			ev := canceledOr(ctx, evidenceFor())
			out[n] = judgeCell(ctx, b, j, tr, ev)
			out[n].Repeat = repeat
			if left[n/repeats].Add(-1) == 0 && ev.dir != "" {
				if err := os.RemoveAll(ev.dir); err != nil {
					notify(batch.Event{Task: tr.ID, Kind: "judge", Text: "judge: leaving its checkout behind: " + err.Error()})
				}
			}
			text := fmt.Sprintf("%s: %d/10 · %s", label, out[n].Score, tools.OneLine(out[n].Reason, 120))
			if !out[n].Scored() {
				text = label + ": " + tools.OneLine(out[n].Error, 160)
			}
			notify(batch.Event{Task: tr.ID, Kind: "judge", Text: text})
		}()
	}
	wg.Wait()
	return out
}

type evidence struct {
	prompt    string
	dir       string
	err       error
	diff      string
	untracked []string
	changes   *changedLines // built once per cell when the judge has criteria
}

// Report cancellation rather than an incidental git failure it caused.
func canceledOr(ctx context.Context, ev evidence) evidence {
	if ev.err != nil && ctx.Err() != nil {
		ev.err = errors.New("canceled")
	}
	return ev
}

func evidenceOf(ctx context.Context, res batch.Result, tr batch.TaskResult, prompt, rubric string, j *Judge) evidence {
	diff, untracked, err := batch.Diff(ctx, res, tr.ID)
	if err != nil {
		return evidence{err: fmt.Errorf("reading the diff: %w", err)}
	}
	var final string
	if n := len(tr.Attempts); n > 0 {
		final = tr.Attempts[n-1].Result.Final
	}
	dir, err := blindCheckout(ctx, res.Dir, res.Base, firstOf(tr.Commit, res.Base))
	if err != nil {
		return evidence{err: fmt.Errorf("preparing the judge's checkout: %w", err)}
	}
	ev := evidence{prompt: judgePrompt(prompt, rubric, final, diff, untracked, j), dir: dir, diff: diff, untracked: untracked}
	if len(j.Criteria) > 0 {
		ev.changes = indexChanges(diff, untracked)
	}
	return ev
}

func judgeCell(ctx context.Context, b agent.Backend, j *Judge, tr batch.TaskResult, ev evidence) Judgement {
	jd := Judgement{Cell: tr.ID}
	if ev.err != nil {
		jd.Error = ev.err.Error()
		return jd
	}

	home, err := os.MkdirTemp("", "hrn-eval-home-")
	if err != nil {
		jd.Error = "preparing the judge's home: " + err.Error()
		return jd
	}
	defer os.RemoveAll(home)

	timeout := time.Duration(j.Timeout)
	if timeout == 0 {
		timeout = defaultJudgeTimeout
	}
	task := agent.Task{
		Knowledge: remote.SearcherFromEnv,
		Prompt:    ev.prompt,
		ChildEnv:  []string{"HRN_HOME=" + home},
		// Neutral read-only copies expose context without revealing the variant.
		Dir: ev.dir,
		Options: agent.Options{Mode: agent.ModeReadOnly, Model: j.Model, Effort: j.Effort,
			Timeout: agent.Duration(timeout), NoInstructions: true},
	}
	rec, err := runlog.Create(b.Name(), task, agent.ResolveSetup(b.Name(), task))
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
	if len(j.Criteria) > 0 {
		score, reason, jd.Criteria, err = parseCriteriaVerdict(r.Final, j.Criteria, ev)
	}
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

const criteriaJudgeInstructions = `You are grading one attempt by a coding agent against each criterion and the rubric.
Judge the change itself: an explanation that claims more than the diff shows earns nothing.
An empty diff is right only when the task needed no change.
Return exactly one verdict for every criterion id. For each met criterion, cite evidence
as a file:line or a quoted diff hunk line. Do not count the Not flag items against the change.

Reply with one line of JSON as the last thing you write, and nothing after it:
{"criteria":[{"id":"...","met":true|false,"evidence":"<file:line or a quoted diff hunk line>"}],"reason":"<one sentence>"}
`

func judgePrompt(prompt, rubric, final, diff string, untracked []string, j *Judge) string {
	var b strings.Builder
	if j != nil && len(j.Criteria) > 0 {
		b.WriteString(criteriaJudgeInstructions)
	} else {
		b.WriteString(judgeInstructions)
	}
	section := func(name, body string) {
		f := fence(body)
		fmt.Fprintf(&b, "\n## %s\n%s\n%s\n%s\n", name, f, body, f)
	}
	section("Task", prompt)
	section("Rubric", rubric)
	if j != nil && len(j.Criteria) > 0 {
		var lines []string
		for _, c := range j.Criteria {
			lines = append(lines, "- "+c.ID+": "+c.Check)
		}
		section("Criteria", strings.Join(lines, "\n"))
		if len(j.NotFlag) > 0 {
			section("Not flag", "- "+strings.Join(j.NotFlag, "\n- "))
		}
	}
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

func clip(s string, limit int) string {
	kept := head(s, limit)
	if len(kept) == len(s) {
		return s
	}
	return kept + fmt.Sprintf("\n[... %d more bytes not shown]", len(s)-len(kept))
}

func head(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func fence(s string) string {
	f := "```"
	for strings.Contains(s, f) {
		f += "`"
	}
	return f
}

// Scan backward because models may wrap the final verdict in prose or fences.
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

func parseCriteriaVerdict(text string, criteria []Criterion, ev evidence) (int, string, []CriterionVerdict, error) {
	changes := ev.changes
	if changes == nil {
		changes = indexChanges(ev.diff, ev.untracked)
	}
	broken := -1
	for i := strings.LastIndex(text, "{"); i >= 0; i = strings.LastIndex(text[:i], "{") {
		var v struct {
			Criteria json.RawMessage `json:"criteria"`
			Reason   string          `json:"reason"`
		}
		decoder := json.NewDecoder(strings.NewReader(text[i:]))
		if err := decoder.Decode(&v); err != nil {
			if broken < 0 && startsCriteriaObject(text[i:]) {
				broken = i
			}
			continue
		}
		if len(v.Criteria) == 0 {
			continue
		}
		if broken >= i+int(decoder.InputOffset()) {
			return 0, "", nil, errors.New("the final criteria object is incomplete or invalid")
		}
		var verdicts []struct {
			ID       string `json:"id"`
			Met      *bool  `json:"met"`
			Evidence string `json:"evidence"`
		}
		if err := json.Unmarshal(v.Criteria, &verdicts); err != nil || len(verdicts) == 0 {
			return 0, "", nil, errors.New("criteria must be a non-empty array")
		}
		wanted := map[string]bool{}
		for _, c := range criteria {
			wanted[c.ID] = true
		}
		byID := map[string]CriterionVerdict{}
		for _, c := range verdicts {
			if !wanted[c.ID] {
				return 0, "", nil, fmt.Errorf("unknown criterion id %q", c.ID)
			}
			if _, ok := byID[c.ID]; ok {
				return 0, "", nil, fmt.Errorf("duplicate criterion id %q", c.ID)
			}
			if c.Met == nil {
				return 0, "", nil, fmt.Errorf("criterion %q has no boolean met", c.ID)
			}
			downgraded := *c.Met && !changes.supports(c.Evidence)
			byID[c.ID] = CriterionVerdict{ID: c.ID, Met: *c.Met && !downgraded, Evidence: clip(c.Evidence, maxEvidence), Downgraded: downgraded}
		}
		var out []CriterionVerdict
		met := 0
		for _, c := range criteria {
			verdict, ok := byID[c.ID]
			if !ok {
				return 0, "", nil, fmt.Errorf("missing criterion id %q", c.ID)
			}
			out = append(out, verdict)
			if verdict.Met {
				met++
			}
		}
		return int(math.Round(10 * float64(met) / float64(len(criteria)))), strings.TrimSpace(v.Reason), out, nil
	}
	return 0, "", nil, errors.New(`the answer has no {"criteria": ...} object`)
}

// changedLines holds a diff's paths with their changed line ranges and its quotable lines.
type changedLines struct {
	files  map[string]*changedFile
	quoted map[string]bool
	gaps   int // most reference boundaries inside one path, such as spaces
}

type changedFile struct {
	hunked bool
	ranges [][2]int
}

func indexChanges(diff string, untracked []string) *changedLines {
	c := &changedLines{files: map[string]*changedFile{}, quoted: map[string]bool{}}
	for _, path := range untracked {
		c.file(normalizePath(path))
	}
	var oldPath, newPath string
	inHunk := false
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			inHunk, oldPath, newPath = false, "", ""
			if pair := diffHeaderPaths.FindStringSubmatch(line); pair != nil {
				oldPath, newPath = diffPath(pair[1], "a/"), diffPath(pair[2], "b/")
				c.file(oldPath)
				c.file(newPath)
			}
		case !inHunk && strings.HasPrefix(line, "--- "):
			oldPath = diffPath(strings.TrimPrefix(line, "--- "), "a/")
			c.file(oldPath)
		case !inHunk && strings.HasPrefix(line, "+++ "):
			newPath = diffPath(strings.TrimPrefix(line, "+++ "), "b/")
			c.file(newPath)
		case strings.HasPrefix(line, "@@ "):
			inHunk = true
			if m := hunkHeader.FindStringSubmatch(line); m != nil {
				c.addRange(newPath, m[3], m[4])
				// A deleted or renamed-away file has only old line numbers to cite.
				if oldPath != newPath {
					c.addRange(oldPath, m[1], m[2])
				}
			}
		case inHunk && len(line) > 0 && strings.ContainsAny(line[:1], "+- "):
			count := 0
			for _, r := range line[1:] {
				if !unicode.IsSpace(r) {
					count++
				}
			}
			if count >= 3 {
				c.quoted[strings.TrimRightFunc(line, unicode.IsSpace)] = true
			}
		}
	}
	return c
}

func normalizePath(path string) string {
	return strings.TrimPrefix(strings.ReplaceAll(path, `\`, "/"), "./")
}

func diffPath(path, prefix string) string {
	if decoded, err := strconv.Unquote(path); err == nil {
		path = decoded
	}
	path = strings.TrimPrefix(path, prefix)
	if path == "/dev/null" {
		return ""
	}
	return normalizePath(path)
}

func (c *changedLines) file(path string) *changedFile {
	if path == "" {
		return nil
	}
	f := c.files[path]
	if f == nil {
		f = &changedFile{}
		c.files[path] = f
		gaps := 0
		for i := range len(path) {
			if referenceBoundary(path[i]) {
				gaps++
			}
		}
		c.gaps = max(c.gaps, gaps)
	}
	return f
}

func (c *changedLines) addRange(path, start, count string) {
	f := c.file(path)
	if f == nil {
		return
	}
	f.hunked = true
	first, err := strconv.Atoi(start)
	n := 1
	if err == nil && count != "" {
		n, err = strconv.Atoi(count)
	}
	if err == nil && n > 0 {
		f.ranges = append(f.ranges, [2]int{first, first + n - 1})
	}
}

// Files the diff shows without hunks (untracked, renamed, binary) accept any positive line.
func (c *changedLines) covers(path string, line int) bool {
	f := c.files[path]
	if f == nil || line < 1 {
		return false
	}
	if !f.hunked {
		return true
	}
	for _, r := range f.ranges {
		if line >= r[0] && line <= r[1] {
			return true
		}
	}
	return false
}

func referenceBoundary(b byte) bool {
	return strings.IndexByte(" \t\n\f\r(\"'`<[{", b) >= 0
}

func (c *changedLines) supports(text string) bool {
	if c.quoted[strings.TrimRightFunc(text, unicode.IsSpace)] {
		return true
	}
	text = strings.ReplaceAll(head(text, maxEvidence), `\`, "/")
	for colon := range len(text) {
		if text[colon] != ':' {
			continue
		}
		end := colon + 1
		for end < len(text) && text[end] >= '0' && text[end] <= '9' {
			end++
		}
		line, err := strconv.Atoi(text[colon+1 : end])
		if err != nil {
			continue
		}
		// A path may hold boundaries itself, so try starts back to as many as any changed path has.
		for start, gaps := colon-1, 0; start >= -1 && gaps <= c.gaps; start-- {
			if start >= 0 && !referenceBoundary(text[start]) {
				continue
			}
			if c.cites(text[start+1:colon], line) {
				return true
			}
			gaps++
		}
	}
	return false
}

func (c *changedLines) cites(ref string, line int) bool {
	ref = strings.TrimPrefix(ref, "./")
	if c.covers(ref, line) {
		return true
	}
	if rest, ok := strings.CutPrefix(ref, "a/"); ok {
		return c.covers(rest, line)
	}
	if rest, ok := strings.CutPrefix(ref, "b/"); ok {
		return c.covers(rest, line)
	}
	return false
}

// A broken replacement must not turn an earlier verdict into the final one.
func startsCriteriaObject(text string) bool {
	d := json.NewDecoder(strings.NewReader(text))
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return false
	}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return false
		}
		if key == "criteria" {
			return true
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return false
		}
	}
	return false
}
