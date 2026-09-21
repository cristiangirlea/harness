// Package batch is the orchestrator: it runs a set of tasks against one git
// repository, N agents at a time, each in its own worktree on its own branch.
// After an agent finishes, gate commands (build, test) run in that worktree;
// a task whose run or gates fail is retried on the next backend in its list.
// Every attempt is an ordinary recorded run, so the eval layer can score it.
package batch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/mcp"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/tools"
)

// Spec is a batch of tasks to run against one repository. Task fields
// override the Spec defaults of the same name.
type Spec struct {
	Dir            string   `json:"dir,omitempty"`      // any path inside the repository; default: the spec file's directory
	Parallel       int      `json:"parallel,omitempty"` // agents at once; default 2
	Backend        string   `json:"backend,omitempty"`  // default: api when credentials exist, else claude
	Model          string   `json:"model,omitempty"`
	Mode           string   `json:"mode,omitempty"` // default auto; nothing can approve prompts in a batch
	MaxTurns       int      `json:"max_turns,omitempty"`
	Effort         string   `json:"effort,omitempty"`
	MCP            []string `json:"mcp,omitempty"`             // MCP config files, relative to Dir
	NoInstructions bool     `json:"no_instructions,omitempty"` // skip the project instruction files (api backend)
	Timeout        string   `json:"timeout,omitempty"`         // per attempt, e.g. "20m"
	MaxCostUSD     float64  `json:"max_cost_usd,omitempty"`    // per attempt, api backend only
	TaskBudget     int      `json:"task_budget,omitempty"`     // tokens, api backend only
	Gate           []string `json:"gate,omitempty"`            // commands that must exit 0 in the worktree after the agent finishes
	Retry          []string `json:"retry,omitempty"`           // backends to try, in order, after a failed attempt
	Tasks          []Task   `json:"tasks"`
}

type Task struct {
	ID       string `json:"id,omitempty"` // branch hx/<id> and worktree .hx/worktrees/<id>; default t1, t2, ...
	Prompt   string `json:"prompt"`
	Backend  string `json:"backend,omitempty"`
	Model    string `json:"model,omitempty"`
	Mode     string `json:"mode,omitempty"`
	MaxTurns int    `json:"max_turns,omitempty"`
	Effort   string `json:"effort,omitempty"`
	// NoInstructions adds to the spec-wide switch; a task can turn the project
	// instructions off but not back on.
	NoInstructions bool     `json:"no_instructions,omitempty"`
	Timeout        string   `json:"timeout,omitempty"`
	MaxCostUSD     float64  `json:"max_cost_usd,omitempty"`
	TaskBudget     int      `json:"task_budget,omitempty"`
	Gate           []string `json:"gate,omitempty"`
	Retry          []string `json:"retry,omitempty"`
}

type Status string

const (
	StatusRunning  Status = "running"  // in the record while the batch is still going
	StatusPass     Status = "pass"     // the run finished and every gate exited 0
	StatusFail     Status = "fail"     // every attempt failed its run or a gate
	StatusError    Status = "error"    // setup or bookkeeping failed (branch taken, git failure, commit refused)
	StatusCanceled Status = "canceled" // interrupted
)

// Result is the record of one batch, saved under $HX_HOME/batches before the
// first task starts and again when the batch ends.
type Result struct {
	ID       string        `json:"id"`
	Started  time.Time     `json:"started"`
	Dir      string        `json:"dir"`  // repository root
	Base     string        `json:"base"` // commit every worktree branched from
	Parallel int           `json:"parallel"`
	Tasks    []TaskResult  `json:"tasks"`
	Duration time.Duration `json:"duration_ns"`
}

type TaskResult struct {
	ID       string        `json:"id"`
	Prompt   string        `json:"prompt"`
	Status   Status        `json:"status"`
	Branch   string        `json:"branch"`
	Worktree string        `json:"worktree"`
	Commit   string        `json:"commit,omitempty"` // the agent's changes; empty when it changed nothing
	Attempts []Attempt     `json:"attempts"`
	Error    string        `json:"error,omitempty"`
	Duration time.Duration `json:"duration_ns"`
}

type Attempt struct {
	Backend string       `json:"backend"`
	RunID   string       `json:"run_id,omitempty"`
	Result  agent.Result `json:"result"`
	Gates   []GateResult `json:"gates,omitempty"`
	Error   string       `json:"error,omitempty"`
	// Prompt is what this attempt was actually sent; a retry carries the
	// previous failure. Continued means it reused the worktree.
	Prompt    string `json:"prompt,omitempty"`
	Continued bool   `json:"continued,omitempty"`
}

type GateResult struct {
	Command  string        `json:"command"`
	ExitCode int           `json:"exit_code"` // -1 when killed (timeout or cancellation)
	Output   string        `json:"output"`    // last part of combined stdout/stderr
	Duration time.Duration `json:"duration_ns"`
}

// Event is progress for a caller to print. Kind is one of start, tool, run,
// gate, retry, done. Run delivers events one at a time, so the callback need
// not lock.
type Event struct {
	Task string
	Kind string
	Text string
}

const gateTimeout = 15 * time.Minute

var idRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// LoadSpec reads a spec file. A relative Dir is taken from the file's
// directory, which is also the default.
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
	if err := spec.normalize(); err != nil {
		return Spec{}, fmt.Errorf("%s: %w", path, err)
	}
	return spec, nil
}

// normalize fills defaults and rejects anything that would only surface after
// an agent has already spent minutes: unknown backends, blank gates, bad ids.
func (s *Spec) normalize() error {
	if s.Dir == "" {
		s.Dir = "."
	}
	if s.Parallel <= 0 {
		s.Parallel = 2
	}
	if s.Mode == "" {
		s.Mode = string(agent.ModeAuto)
	}
	if _, err := agent.ParseMode(s.Mode); err != nil {
		return err
	}
	if err := checkBackends(s.Backend, s.Retry); err != nil {
		return err
	}
	if err := checkGates(s.Gate); err != nil {
		return err
	}
	if err := checkLimits(s.Timeout, s.TaskBudget); err != nil {
		return err
	}
	if len(s.Tasks) == 0 {
		return errors.New("no tasks")
	}
	seen := map[string]bool{}
	for i := range s.Tasks {
		t := &s.Tasks[i]
		if strings.TrimSpace(t.Prompt) == "" {
			return fmt.Errorf("task %d: empty prompt", i+1)
		}
		if t.ID == "" {
			t.ID = fmt.Sprintf("t%d", i+1)
		}
		if !idRE.MatchString(t.ID) {
			return fmt.Errorf("task id %q: use letters, digits, '.', '_' and '-'", t.ID)
		}
		if seen[t.ID] {
			return fmt.Errorf("duplicate task id %q", t.ID)
		}
		seen[t.ID] = true
		if t.Mode != "" {
			if _, err := agent.ParseMode(t.Mode); err != nil {
				return fmt.Errorf("task %s: %w", t.ID, err)
			}
		}
		if err := checkBackends(t.Backend, t.Retry); err != nil {
			return fmt.Errorf("task %s: %w", t.ID, err)
		}
		if err := checkGates(t.Gate); err != nil {
			return fmt.Errorf("task %s: %w", t.ID, err)
		}
		if err := checkLimits(t.Timeout, t.TaskBudget); err != nil {
			return fmt.Errorf("task %s: %w", t.ID, err)
		}
	}
	return nil
}

func checkBackends(backend string, retry []string) error {
	for _, name := range append([]string{backend}, retry...) {
		if name == "" {
			continue
		}
		if _, err := agent.Get(name); err != nil {
			return err
		}
	}
	return nil
}

// checkLimits rejects a bad duration or task budget before anything runs.
func checkLimits(timeout string, taskBudget int) error {
	if timeout != "" {
		if _, err := time.ParseDuration(timeout); err != nil {
			return fmt.Errorf("timeout %q: %w", timeout, err)
		}
	}
	return agent.ValidateTaskBudget(taskBudget)
}

func checkGates(gates []string) error {
	for _, g := range gates {
		if strings.TrimSpace(g) == "" {
			return errors.New("empty gate command")
		}
	}
	return nil
}

// Run executes the batch and saves its record. Tasks run through a pool of
// Parallel workers; cancelling ctx stops running agents and skips the rest.
// The record is written before the first task starts, so `hx batch rm` can
// clean up even after a crash, and again at the end. A failure to write the
// final record is returned together with the finished Result.
func Run(ctx context.Context, spec Spec, notify func(Event)) (Result, error) {
	started := time.Now()
	if err := spec.normalize(); err != nil {
		return Result{}, err
	}
	// Workers and agents report concurrently; callers get one event at a time.
	report := notify
	if report == nil {
		report = func(Event) {}
	}
	var notifyMu sync.Mutex
	notify = func(e Event) {
		notifyMu.Lock()
		defer notifyMu.Unlock()
		report(e)
	}

	root, err := gitOut(ctx, spec.Dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return Result{}, err
	}
	root = filepath.Clean(filepath.FromSlash(root))
	base, err := gitOut(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return Result{}, err
	}
	var mcpPaths []string
	for _, p := range spec.MCP {
		if !filepath.IsAbs(p) {
			p = filepath.Join(spec.Dir, p)
		}
		mcpPaths = append(mcpPaths, p)
	}
	if mcpPaths, err = mcp.ConfigPaths(mcpPaths); err != nil {
		return Result{}, err
	}
	// Keep the worktrees out of the repository's status.
	wtRoot := filepath.Join(root, ".hx", "worktrees")
	if err := os.MkdirAll(wtRoot, 0o755); err != nil {
		return Result{}, err
	}
	if _, err := os.Stat(filepath.Join(root, ".hx", ".gitignore")); err != nil {
		_ = os.WriteFile(filepath.Join(root, ".hx", ".gitignore"), []byte("*\n"), 0o644)
	}

	res := Result{ID: runlog.NewID(), Started: started, Dir: root, Base: base, Parallel: spec.Parallel, Tasks: make([]TaskResult, len(spec.Tasks))}
	for i, t := range spec.Tasks {
		res.Tasks[i] = TaskResult{ID: t.ID, Prompt: t.Prompt, Status: StatusRunning, Branch: "hx/" + t.ID,
			Worktree: filepath.Join(wtRoot, t.ID), Attempts: []Attempt{}}
	}
	if err := save(res); err != nil {
		return Result{}, fmt.Errorf("saving batch record: %w", err)
	}

	idx := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < spec.Parallel; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range idx {
				res.Tasks[i] = runTask(ctx, &spec, spec.Tasks[i], res.Tasks[i], root, base, mcpPaths, notify)
			}
		}()
	}
	for i := range spec.Tasks {
		idx <- i
	}
	close(idx)
	wg.Wait()
	res.Duration = time.Since(started)
	if err := save(res); err != nil {
		return res, fmt.Errorf("saving batch record: %w", err)
	}
	return res, nil
}

// attemptPlan is how one attempt differs from the one before it.
type attemptPlan struct {
	number int    // 1-based
	prompt string // what the agent is sent, failure context included
	commit string // the commit subject for whatever it leaves behind
	// continued keeps the worktree and the work already committed on the
	// branch, so the agent fixes its own output instead of starting over.
	continued bool
}

// The prompt a retry gets. A gate failure is recoverable in place, so the next
// attempt is told what failed and that the work is already there; anything
// else starts over and is told only how the previous attempt ended.
const (
	gateFailureContext = "\n\n---\n" +
		"A previous attempt at this task (attempt %d on %s) did not pass. Its changes are\n" +
		"already applied in this workspace and committed on the current branch. Fix them\n" +
		"rather than starting over.\n\n" +
		"The gate command `%s` exited %d. Output (tail):\n" +
		"%s\n%s\n%s\n"
	runFailureContext = "\n\n---\n" +
		"A previous attempt on %s ended with %s: %s\n" +
		"This workspace is a fresh copy; nothing from that attempt is in it.\n"
	attemptHistory = "\nEarlier attempts, so they are not tried again: %s.\n"
)

// failedGate returns the gate that stopped an attempt, or nil when none did.
func failedGate(att Attempt) *GateResult {
	for i := range att.Gates {
		if att.Gates[i].ExitCode != 0 {
			return &att.Gates[i]
		}
	}
	return nil
}

// retryContext is what the next attempt is told, and whether it may continue in
// the same worktree. It takes every attempt so far, not just the last, so a
// third attempt knows what the second already tried.
func retryContext(attempts []Attempt) (text string, continued bool) {
	last := attempts[len(attempts)-1]
	if g := failedGate(last); last.Result.Status == agent.StatusOK && g != nil {
		f := fence(g.Output)
		text = fmt.Sprintf(gateFailureContext, len(attempts), last.Backend, g.Command, g.ExitCode, f, g.Output, f)
		continued = true
	} else {
		text = fmt.Sprintf(runFailureContext, last.Backend, last.Result.Status, last.Result.Error)
	}
	if earlier := attempts[:len(attempts)-1]; len(earlier) > 0 {
		summaries := make([]string, 0, len(earlier))
		for i, att := range earlier {
			summaries = append(summaries, fmt.Sprintf("%d on %s %s", i+1, att.Backend, outcomeOf(att)))
		}
		text += fmt.Sprintf(attemptHistory, strings.Join(summaries, "; "))
	}
	return text, continued
}

// outcomeOf is one attempt's fate in a few words.
func outcomeOf(att Attempt) string {
	if g := failedGate(att); g != nil {
		return fmt.Sprintf("(gate `%s` exited %d)", tools.OneLine(g.Command, 60), g.ExitCode)
	}
	if att.Result.Status != agent.StatusOK {
		return fmt.Sprintf("(%s: %s)", att.Result.Status, tools.OneLine(att.Result.Error, 80))
	}
	return "(passed)"
}

// fence returns a code fence longer than any run of backticks in s, so quoted
// command output cannot close it early.
func fence(s string) string {
	f := "```"
	for strings.Contains(s, f) {
		f += "`"
	}
	return f
}

func runTask(ctx context.Context, spec *Spec, t Task, tr TaskResult, root, base string, mcpPaths []string, notify func(Event)) TaskResult {
	started := time.Now()
	tr.Status, tr.Attempts, tr.Commit, tr.Error = "", []Attempt{}, "", ""
	finish := func(status Status, text string) TaskResult {
		tr.Status = status
		tr.Duration = time.Since(started)
		notify(Event{Task: t.ID, Kind: "done", Text: text})
		return tr
	}
	fail := func(err error) TaskResult {
		if ctx.Err() != nil {
			return finish(StatusCanceled, "canceled")
		}
		tr.Error = err.Error()
		return finish(StatusError, "error: "+err.Error())
	}
	if ctx.Err() != nil {
		return finish(StatusCanceled, "canceled")
	}

	backends := []string{firstOf(t.Backend, spec.Backend, agent.DefaultBackendName())}
	backends = append(backends, firstList(t.Retry, spec.Retry)...)
	gates := firstList(t.Gate, spec.Gate)
	mode, _ := agent.ParseMode(firstOf(t.Mode, spec.Mode))

	if _, err := gitOut(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+tr.Branch); err == nil {
		return fail(fmt.Errorf("branch %s already exists; remove it or pick another task id", tr.Branch))
	}
	if _, err := os.Stat(tr.Worktree); err == nil {
		return fail(fmt.Errorf("worktree %s already exists; remove it or pick another task id", tr.Worktree))
	}

	task := agent.Task{
		Dir: tr.Worktree, Mode: mode, MCP: mcpPaths,
		NoInstructions: t.NoInstructions || spec.NoInstructions,
		Timeout:        duration(firstOf(t.Timeout, spec.Timeout)),
		MaxCostUSD:     firstFloat(t.MaxCostUSD, spec.MaxCostUSD),
		TaskBudget:     firstInt(t.TaskBudget, spec.TaskBudget),
		Model:          firstOf(t.Model, spec.Model), Effort: firstOf(t.Effort, spec.Effort),
		MaxTurns: t.MaxTurns,
	}
	if task.MaxTurns == 0 {
		task.MaxTurns = spec.MaxTurns
	}

	for i, name := range backends {
		b, err := agent.Get(name)
		if err != nil {
			return fail(err)
		}
		plan := attemptPlan{number: i + 1, prompt: t.Prompt}
		if i > 0 {
			text, continued := retryContext(tr.Attempts)
			plan.prompt, plan.continued = t.Prompt+text, continued
			where := "fresh worktree"
			if continued {
				where = "continuing in worktree"
			}
			notify(Event{Task: t.ID, Kind: "retry", Text: fmt.Sprintf("retrying on %s (%s)", name, where)})
		}
		plan.commit = t.ID + ": " + tools.OneLine(t.Prompt, 60)
		if plan.continued {
			plan.commit += fmt.Sprintf(" (attempt %d)", plan.number)
		}
		if !plan.continued {
			if i > 0 {
				if err := removeWorktree(root, tr); err != nil {
					return fail(err)
				}
			}
			tr.Commit = ""
			if _, err := gitOut(ctx, root, "worktree", "add", "-q", "-b", tr.Branch, tr.Worktree, base); err != nil {
				return fail(err)
			}
		}
		notify(Event{Task: t.ID, Kind: "start", Text: fmt.Sprintf("%s in %s", name, tr.Worktree)})

		out, err := runAttempt(ctx, b, t.ID, task, plan, gates, notify)
		tr.Attempts = append(tr.Attempts, out.att)
		if out.commit != "" {
			tr.Commit = out.commit
		}
		switch {
		case err != nil:
			return fail(err)
		case out.pass:
			text := "pass · branch " + tr.Branch
			if tr.Commit != "" {
				text += " · commit " + short(tr.Commit)
			}
			return finish(StatusPass, text)
		case ctx.Err() != nil:
			return finish(StatusCanceled, "canceled")
		}
	}
	return finish(StatusFail, fmt.Sprintf("fail after %d attempt(s) · worktree kept at %s", len(tr.Attempts), tr.Worktree))
}

type attemptOutcome struct {
	att    Attempt
	commit string // empty when the agent changed nothing
	pass   bool   // the run finished and every gate exited 0
}

// runAttempt runs the agent in the task's worktree, then the gates, then
// commits whatever was left behind, pass or fail. A returned error aborts the
// whole task; a failed run or gate is reported in the outcome instead.
func runAttempt(ctx context.Context, b agent.Backend, id string, task agent.Task, plan attemptPlan, gates []string, notify func(Event)) (attemptOutcome, error) {
	task.Prompt = plan.prompt
	att := Attempt{Backend: b.Name(), Prompt: plan.prompt, Continued: plan.continued}

	rec, err := runlog.Create(b.Name(), task)
	if err != nil {
		return attemptOutcome{att: att}, fmt.Errorf("creating run record: %w", err)
	}
	att.RunID = rec.ID()
	att.Result = b.Run(ctx, task, func(e agent.Event) {
		rec.Event(e)
		if e.Kind == agent.EventToolCall {
			notify(Event{Task: id, Kind: "tool", Text: strings.TrimSpace("→ " + e.Tool + " " + tools.OneLine(e.Text, 120))})
		}
	})
	if err := rec.Close(att.Result); err != nil {
		att.Error = "saving run: " + err.Error()
	}
	text := fmt.Sprintf("%s · %d turns · run %s", att.Result.Status, att.Result.Turns, att.RunID)
	if att.Result.Error != "" {
		text += " · " + tools.OneLine(att.Result.Error, 160)
	}
	notify(Event{Task: id, Kind: "run", Text: text})

	pass := att.Result.Status == agent.StatusOK
	if pass {
		for _, g := range gates {
			gr := runGate(ctx, task.Dir, g)
			att.Gates = append(att.Gates, gr)
			if gr.ExitCode != 0 {
				pass = false
				notify(Event{Task: id, Kind: "gate", Text: fmt.Sprintf("✗ %s (exit %d)", g, gr.ExitCode)})
				break
			}
			notify(Event{Task: id, Kind: "gate", Text: "✓ " + g})
		}
	}

	// The branch holds whatever the agent left, pass or fail, so the worktree
	// can be inspected or removed without losing work.
	sha, commitErr := commitChanges(task.Dir, plan.commit)
	if commitErr != nil {
		att.Error = strings.TrimSpace(att.Error + "\ncommit: " + commitErr.Error())
		// The work exists only in the worktree; say so instead of claiming a pass.
		return attemptOutcome{att: att}, fmt.Errorf("commit failed, changes are uncommitted in %s: %v", task.Dir, commitErr)
	}
	return attemptOutcome{att: att, commit: sha, pass: pass}, nil
}

func runGate(ctx context.Context, dir, command string) GateResult {
	started := time.Now()
	res, err := tools.RunShell(ctx, dir, command, gateTimeout)
	gr := GateResult{Command: command, ExitCode: res.ExitCode, Output: tail(res.Output, 4000), Duration: time.Since(started)}
	switch {
	case err != nil:
		gr.ExitCode = -1
		gr.Output = strings.TrimSpace(gr.Output + "\n" + err.Error())
	case res.TimedOut:
		gr.Output = strings.TrimSpace(gr.Output + fmt.Sprintf("\n[timed out after %s]", gateTimeout))
	}
	return gr
}

// commitChanges commits everything the agent left in the worktree and returns
// the commit, or "" when nothing changed. It runs after the agent, so it uses
// its own context: a cancelled batch must still leave a consistent branch.
func commitChanges(wt, message string) (string, error) {
	dirty, err := hasChanges(wt)
	if err != nil || !dirty {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := gitOut(ctx, wt, "add", "-A"); err != nil {
		return "", err
	}
	if _, err := gitOut(ctx, wt, "commit", "-q", "-m", message); err != nil {
		return "", err
	}
	return gitOut(ctx, wt, "rev-parse", "HEAD")
}

// hasChanges reports whether a worktree holds uncommitted work. A missing
// worktree holds none.
func hasChanges(wt string) (bool, error) {
	if _, err := os.Stat(wt); err != nil {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	status, err := gitOut(ctx, wt, "status", "--porcelain")
	return status != "", err
}

// Remove deletes the worktrees and branches a batch created. A worktree with
// uncommitted changes is left alone and reported, because the branch would
// not hold that work. The record and the run logs stay.
func Remove(res Result) error {
	var errs []error
	for _, tr := range res.Tasks {
		if tr.Worktree == "" {
			continue
		}
		if dirty, err := hasChanges(tr.Worktree); err == nil && dirty {
			errs = append(errs, fmt.Errorf("%s: %s has uncommitted changes; commit or discard them first", tr.ID, tr.Worktree))
			continue
		}
		if err := removeWorktree(res.Dir, tr); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", tr.ID, err))
		}
	}
	return errors.Join(errs...)
}

func removeWorktree(root string, tr TaskResult) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := os.Stat(tr.Worktree); err == nil {
		if _, err := gitOut(ctx, root, "worktree", "remove", "--force", tr.Worktree); err != nil {
			return err
		}
	} else {
		_, _ = gitOut(ctx, root, "worktree", "prune")
	}
	if _, err := gitOut(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+tr.Branch); err == nil {
		if _, err := gitOut(ctx, root, "branch", "-D", tr.Branch); err != nil {
			return err
		}
	}
	return nil
}

func batchesDir() string { return filepath.Join(runlog.Home(), "batches") }

func save(res Result) error {
	if err := os.MkdirAll(batchesDir(), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(batchesDir(), res.ID+".json"), data, 0o644)
}

// LoadResult reads a saved batch record.
func LoadResult(id string) (Result, error) {
	var res Result
	if id == "" || strings.ContainsAny(id, `/\`) {
		return res, fmt.Errorf("invalid batch id %q", id)
	}
	data, err := os.ReadFile(filepath.Join(batchesDir(), id+".json"))
	if err != nil {
		return res, err
	}
	return res, json.Unmarshal(data, &res)
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), text)
	}
	return text, nil
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// duration parses a spec duration that normalize has already validated.
func duration(s string) time.Duration {
	d, _ := time.ParseDuration(s)
	return d
}

func firstFloat(values ...float64) float64 {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}

func firstInt(values ...int) int {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}

func firstList(lists ...[]string) []string {
	for _, l := range lists {
		if l != nil {
			return l
		}
	}
	return nil
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
