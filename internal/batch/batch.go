// Package batch is the orchestrator: it runs a set of tasks against one git
// repository, N agents at a time, each in its own worktree on its own branch.
// After an agent finishes, gate commands (build, test) run in that worktree;
// a task whose run or gates fail is retried on the next backend in its list.
// Every attempt is an ordinary recorded run, so the eval layer can score it.
package batch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
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
	Dir      string `json:"dir,omitempty"`      // any path inside the repository; default: the spec file's directory
	Parallel int    `json:"parallel,omitempty"` // agents at once; default 2
	Backend  string `json:"backend,omitempty"`  // default: api when credentials exist, else claude
	// Options are the run options every task starts from: model, mode (default
	// auto; nothing can approve prompts in a batch), the budgets (per attempt),
	// MCP files (relative to Dir) and the api switches. A task's own override
	// them field by field.
	agent.Options
	AllowDirty bool     `json:"allow_dirty,omitempty"` // start even though tracked files are modified
	Gate       []string `json:"gate,omitempty"`        // commands that must exit 0 in the worktree after the agent finishes
	Retry      []string `json:"retry,omitempty"`       // backends to try, in order, after a failed attempt
	Tasks      []Task   `json:"tasks"`
}

type Task struct {
	ID      string `json:"id,omitempty"` // branch hrn/<id> and worktree .hrn/worktrees/<id>; default t1, t2, ...
	Prompt  string `json:"prompt"`
	Backend string `json:"backend,omitempty"`
	// Options set here override the spec's, field by field; a switch can be
	// turned on for one task but not back off, and a zero limit inherits.
	agent.Options
	Gate  []string `json:"gate,omitempty"`
	Retry []string `json:"retry,omitempty"`
	// Expect is a regular expression the agent's final answer must match. It
	// is checked before the gates and reported as one, so a task whose answer
	// is the deliverable ("is there a bug?") can fail on what it said.
	Expect string `json:"expect,omitempty"`
}

type Status string

const (
	StatusRunning  Status = "running"  // in the record while the batch is still going
	StatusPass     Status = "pass"     // the run finished and every gate exited 0
	StatusFail     Status = "fail"     // every attempt failed its run or a gate
	StatusError    Status = "error"    // setup or bookkeeping failed (branch taken, git failure, commit refused)
	StatusCanceled Status = "canceled" // interrupted
)

// Result is the record of one batch, saved under $HRN_HOME/batches before the
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

// worktreeMu serialises `git worktree add`, `remove` and `prune` for this
// process. They all rewrite the repository's shared worktree metadata, and git
// takes no lock of its own around it, so two workers doing it at once can fail
// one another's command. Everything else the workers run - the agent, the
// gates, the commit - stays parallel, which is where the time goes.
var worktreeMu sync.Mutex

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
		s.Mode = agent.ModeAuto
	}
	if err := s.Options.Validate(); err != nil {
		return err
	}
	if err := checkBackends(s.Backend, s.Retry); err != nil {
		return err
	}
	if err := checkGates(s.Gate); err != nil {
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
		if err := t.Options.Validate(); err != nil {
			return fmt.Errorf("task %s: %w", t.ID, err)
		}
		if err := checkBackends(t.Backend, t.Retry); err != nil {
			return fmt.Errorf("task %s: %w", t.ID, err)
		}
		if err := checkGates(t.Gate); err != nil {
			return fmt.Errorf("task %s: %w", t.ID, err)
		}
		if _, err := compileExpect(t.Expect); err != nil {
			return fmt.Errorf("task %s: %w", t.ID, err)
		}
	}
	return nil
}

// compileExpect compiles a task's expect pattern; nil when there is none.
func compileExpect(expr string) (*regexp.Regexp, error) {
	if expr == "" {
		return nil, nil
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("expect: %w", err)
	}
	return re, nil
}

// expectCommand is how an expect check is named where a gate command would be.
func expectCommand(expr string) string { return "expect /" + expr + "/" }

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

func checkGates(gates []string) error {
	for _, g := range gates {
		if strings.TrimSpace(g) == "" {
			return errors.New("empty gate command")
		}
	}
	return nil
}

// checkClean refuses to start when tracked files are modified: every worktree
// branches from HEAD, so that work would not reach the agents and the batch
// would quietly be testing something other than what the operator sees.
// Untracked files are not counted - .hrn/worktrees is one of them.
func checkClean(ctx context.Context, root string) error {
	lines, err := dirtyPaths(ctx, root, false)
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		return nil
	}
	shown, more := lines, ""
	if len(shown) > 5 {
		shown, more = shown[:5], fmt.Sprintf("\n  ... and %d more", len(lines)-5)
	}
	return fmt.Errorf("the repository has uncommitted changes; the agents branch from HEAD and would not see them. "+
		"Commit or stash them, or pass --allow-dirty (allow_dirty in the spec):\n  %s%s",
		strings.Join(shown, "\n  "), more)
}

// Run executes the batch and saves its record. Tasks run through a pool of
// Parallel workers; cancelling ctx stops running agents and skips the rest.
// The record is written before the first task starts, so `hrn batch rm` can
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
	if !spec.AllowDirty {
		if err := checkClean(ctx, root); err != nil {
			return Result{}, err
		}
	}
	if spec.MCP, err = mcpPathsOf(spec.Dir, spec.MCP); err != nil {
		return Result{}, err
	}
	// A task's own list is checked here too, so a typo in task seven does not
	// surface after six agents have run.
	for i := range spec.Tasks {
		if t := &spec.Tasks[i]; t.MCP != nil {
			if t.MCP, err = mcpPathsOf(spec.Dir, t.MCP); err != nil {
				return Result{}, fmt.Errorf("task %s: %w", t.ID, err)
			}
		}
	}
	// Keep the worktrees out of the repository's status.
	wtRoot := filepath.Join(root, ".hrn", "worktrees")
	if err := os.MkdirAll(wtRoot, 0o755); err != nil {
		return Result{}, err
	}
	if _, err := os.Stat(filepath.Join(root, ".hrn", ".gitignore")); err != nil {
		_ = os.WriteFile(filepath.Join(root, ".hrn", ".gitignore"), []byte("*\n"), 0o644)
	}

	res := Result{ID: runlog.NewID(), Started: started, Dir: root, Base: base, Parallel: spec.Parallel, Tasks: make([]TaskResult, len(spec.Tasks))}
	for i, t := range spec.Tasks {
		res.Tasks[i] = TaskResult{ID: t.ID, Prompt: t.Prompt, Status: StatusRunning, Branch: "hrn/" + t.ID,
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
				res.Tasks[i] = runTask(ctx, &spec, spec.Tasks[i], res.Tasks[i], root, base, notify)
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

func runTask(ctx context.Context, spec *Spec, t Task, tr TaskResult, root, base string, notify func(Event)) TaskResult {
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
	// The spec's options with whatever the task sets on top; Run resolved
	// both MCP lists already.
	opts := t.Options.Over(spec.Options)

	if _, err := gitOut(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+tr.Branch); err == nil {
		return fail(fmt.Errorf("branch %s already exists; remove it or pick another task id", tr.Branch))
	}
	if _, err := os.Stat(tr.Worktree); err == nil {
		return fail(fmt.Errorf("worktree %s already exists; remove it or pick another task id", tr.Worktree))
	}

	task := agent.Task{Dir: tr.Worktree, Options: opts}
	expect, err := compileExpect(t.Expect)
	if err != nil {
		return fail(err)
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
			if err := addWorktree(ctx, root, tr, base); err != nil {
				return fail(err)
			}
		}
		notify(Event{Task: t.ID, Kind: "start", Text: fmt.Sprintf("%s in %s", name, tr.Worktree)})

		out, err := runAttempt(ctx, b, t.ID, task, plan, expect, gates, notify)
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
				text += " · commit " + tools.ShortSHA(tr.Commit)
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
func runAttempt(ctx context.Context, b agent.Backend, id string, task agent.Task, plan attemptPlan, expect *regexp.Regexp, gates []string, notify func(Event)) (attemptOutcome, error) {
	task.Prompt = plan.prompt
	att := Attempt{Backend: b.Name(), Prompt: plan.prompt, Continued: plan.continued}

	rec, err := runlog.Create(b.Name(), task)
	if err != nil {
		return attemptOutcome{att: att}, fmt.Errorf("creating run record: %w", err)
	}
	att.RunID = rec.ID()
	task.RunDir = rec.Dir() // so a finished attempt can be continued
	att.Result = agent.Run(ctx, b, task, func(e agent.Event) {
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
	if pass && expect != nil {
		gr := checkExpect(expect, att.Result.Final)
		att.Gates = append(att.Gates, gr)
		if gr.ExitCode != 0 {
			pass = false
			notify(Event{Task: id, Kind: "gate", Text: "✗ " + gr.Command})
		} else {
			notify(Event{Task: id, Kind: "gate", Text: "✓ " + gr.Command})
		}
	}
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

// checkExpect is the expect pattern run as a gate: exit 0 on a match, 1
// otherwise, with the tail of the answer as the output a retry is shown.
func checkExpect(re *regexp.Regexp, final string) GateResult {
	gr := GateResult{Command: expectCommand(re.String()), Output: tail(final, 4000)}
	if !re.MatchString(final) {
		gr.ExitCode = 1
		if strings.TrimSpace(final) == "" {
			gr.Output = "(the agent gave no final answer)"
		}
	}
	return gr
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
	// Untracked files count here: a worktree holding work the branch does not
	// have must not be removed. Starting a batch ignores them instead, because
	// .hrn/worktrees itself is untracked in the root.
	lines, err := dirtyPaths(ctx, wt, true)
	return len(lines) > 0, err
}

// dirtyPaths lists the porcelain status lines of a working tree, with or
// without untracked files. One place decides what "dirty" means.
func dirtyPaths(ctx context.Context, dir string, untracked bool) ([]string, error) {
	args := []string{"status", "--porcelain"}
	if !untracked {
		args = append(args, "--untracked-files=no")
	}
	status, err := gitOut(ctx, dir, args...)
	if err != nil || status == "" {
		return nil, err
	}
	return strings.Split(status, "\n"), nil
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

// addWorktree creates the task's worktree and branch from the base commit.
func addWorktree(ctx context.Context, root string, tr TaskResult, base string) error {
	worktreeMu.Lock()
	defer worktreeMu.Unlock()
	_, err := gitOut(ctx, root, "worktree", "add", "-q", "-b", tr.Branch, tr.Worktree, base)
	return err
}

func removeWorktree(root string, tr TaskResult) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	worktreeMu.Lock()
	defer worktreeMu.Unlock()
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
	// Written beside the record and renamed over it: `hrn batch show` reads
	// this file while the batch is still running, and half a file is not a
	// record. Rename replaces an existing file on Windows too.
	path := filepath.Join(batchesDir(), res.ID+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// List returns every saved batch record, newest first, and how many files
// could not be read: one bad record must not hide the rest, nor vanish.
func List() (results []Result, unreadable int, err error) {
	entries, err := os.ReadDir(batchesDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if e.IsDir() || !ok {
			continue
		}
		res, err := LoadResult(id)
		if err != nil {
			unreadable++
			continue
		}
		results = append(results, res)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Started.After(results[j].Started) })
	return results, unreadable, nil
}

// Find returns the task with this id.
func (r Result) Find(id string) (TaskResult, error) {
	ids := make([]string, 0, len(r.Tasks))
	for _, tr := range r.Tasks {
		if tr.ID == id {
			return tr, nil
		}
		ids = append(ids, tr.ID)
	}
	return TaskResult{}, fmt.Errorf("batch %s has no task %q (it has %s)", r.ID, id, strings.Join(ids, ", "))
}

// Diff returns what a task changed: its commit against the base the batch
// started from, or whatever is still uncommitted in its worktree. Untracked
// paths come back separately, because a diff cannot show them.
func Diff(ctx context.Context, res Result, taskID string) (diff string, untracked []string, err error) {
	tr, err := res.Find(taskID)
	if err != nil {
		return "", nil, err
	}
	if tr.Commit != "" {
		// `hrn batch rm` deletes the branch, and the commit goes with it once
		// git collects it; say that rather than passing on "bad object".
		if _, err := gitOut(ctx, res.Dir, "cat-file", "-e", tr.Commit+"^{commit}"); err != nil {
			return "", nil, fmt.Errorf("task %s: commit %s is no longer in %s, so its branch was removed",
				taskID, tools.ShortSHA(tr.Commit), res.Dir)
		}
		out, err := gitStdout(ctx, res.Dir, "diff", res.Base+".."+tr.Commit)
		return out, nil, err
	}
	if _, err := os.Stat(tr.Worktree); err != nil {
		return "", nil, fmt.Errorf("task %s committed nothing and its worktree is gone", taskID)
	}
	out, err := gitStdout(ctx, tr.Worktree, "diff", "HEAD")
	if err != nil {
		return "", nil, err
	}
	lines, err := dirtyPaths(ctx, tr.Worktree, true)
	for _, l := range lines {
		if rest, ok := strings.CutPrefix(l, "?? "); ok {
			untracked = append(untracked, rest)
		}
	}
	return out, untracked, err
}

// MergeResult is what one task's branch did.
type MergeResult struct {
	Task     string `json:"task"`
	Branch   string `json:"branch"`
	Commit   string `json:"commit,omitempty"`   // the merge or squash commit this run made
	Skipped  string `json:"skipped,omitempty"`  // why nothing was merged
	Conflict bool   `json:"conflict,omitempty"` // git reported conflicting paths
	Error    string `json:"error,omitempty"`    // any other failure, in git's words
}

// Merge merges the branches of a batch into whatever the repository has
// checked out, in spec order, and never pushes. With no names it takes every
// task that passed; named tasks are still only merged if they passed, so
// nothing that failed its gates arrives by accident. It stops at the first
// failure and leaves the tree as it found it. The results so far come back
// with the error.
func Merge(ctx context.Context, res Result, only []string, squash bool) ([]MergeResult, error) {
	if res.Dir == "" {
		return nil, fmt.Errorf("batch %s has no repository recorded", res.ID)
	}
	tasks, err := mergeTasks(res, only)
	if err != nil {
		return nil, err
	}
	lines, err := dirtyPaths(ctx, res.Dir, false)
	if err != nil {
		return nil, err
	}
	if len(lines) > 0 {
		return nil, fmt.Errorf("%s has uncommitted changes; merging would mix them with the agents' work. "+
			"Commit or stash them first:\n  %s", res.Dir, strings.Join(lines, "\n  "))
	}

	var out []MergeResult
	for _, tr := range tasks {
		mr := MergeResult{Task: tr.ID, Branch: tr.Branch}
		switch {
		case tr.Status != StatusPass:
			mr.Skipped = "did not pass (" + string(tr.Status) + ")"
		case tr.Commit == "":
			mr.Skipped = "changed nothing"
		case alreadyIn(ctx, res.Dir, tr.Branch):
			mr.Skipped = "already merged"
		}
		if mr.Skipped != "" {
			out = append(out, mr)
			continue
		}
		commit, conflicted, err := mergeBranch(ctx, res.Dir, tr, squash)
		switch {
		case errors.Is(err, errNothingStaged):
			// A squash of work that is already in the tree stages nothing.
			mr.Skipped = "already merged"
		case err != nil:
			mr.Conflict, mr.Error = conflicted, err.Error()
			out = append(out, mr)
			return out, fmt.Errorf("%s: %w", tr.ID, err)
		default:
			mr.Commit = commit
		}
		out = append(out, mr)
	}
	return out, nil
}

// alreadyIn reports whether every commit of the branch is in HEAD already.
func alreadyIn(ctx context.Context, root, branch string) bool {
	_, err := gitOut(ctx, root, "merge-base", "--is-ancestor", branch, "HEAD")
	return err == nil
}

// mergeTasks picks what to merge: the named tasks, or every task that passed.
func mergeTasks(res Result, only []string) ([]TaskResult, error) {
	if len(only) == 0 {
		var out []TaskResult
		for _, tr := range res.Tasks {
			if tr.Status == StatusPass {
				out = append(out, tr)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("batch %s has no task that passed", res.ID)
		}
		return out, nil
	}
	var out []TaskResult
	for _, id := range only {
		tr, err := res.Find(id)
		if err != nil {
			return nil, err
		}
		out = append(out, tr)
	}
	return out, nil
}

// errNothingStaged says a squash merge brought nothing this tree did not have.
var errNothingStaged = errors.New("nothing to commit")

// mergeBranch merges one branch, reporting whether git left conflicting paths
// behind, which is a different thing from a merge that failed for any other
// reason: a hook refusing the commit, a missing identity, a cancelled run.
func mergeBranch(ctx context.Context, root string, tr TaskResult, squash bool) (commit string, conflicted bool, err error) {
	args := []string{"merge", "--no-ff", "--no-edit", tr.Branch}
	if squash {
		args = []string{"merge", "--squash", tr.Branch}
	}
	if _, err := gitOut(ctx, root, args...); err != nil {
		return "", hasUnmerged(root), undoMerge(root, err)
	}
	if squash {
		if staged, err := hasStagedChanges(ctx, root); err != nil {
			return "", false, undoMerge(root, err)
		} else if !staged {
			return "", false, undoMerge(root, errNothingStaged)
		}
		if _, err := gitOut(ctx, root, "commit", "-q", "-m", tr.ID+": "+tools.OneLine(tr.Prompt, 60)); err != nil {
			return "", hasUnmerged(root), undoMerge(root, err)
		}
	}
	commit, err = gitOut(ctx, root, "rev-parse", "HEAD")
	return commit, false, err
}

// hasUnmerged reports whether the index holds conflicting paths. It takes its
// own context because it runs while a merge is being cleaned up.
func hasUnmerged(root string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := gitOut(ctx, root, "ls-files", "--unmerged")
	return err == nil && out != ""
}

func hasStagedChanges(ctx context.Context, root string) (bool, error) {
	// --quiet exits 1 when there is a difference, which gitOut reports as an
	// error, so ask for the names instead and read them.
	out, err := gitOut(ctx, root, "diff", "--cached", "--name-only")
	return out != "", err
}

// undoMerge puts the tree back and returns the failure that got us here. It
// runs on its own context, because the usual reason a merge failed is that the
// operator interrupted it, and cleanup on a cancelled context does nothing. A
// squash merge leaves no MERGE_HEAD to abort, so HEAD is the way back; the
// tree was clean before the merge, which is what makes that safe.
func undoMerge(root string, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := gitOut(ctx, root, "merge", "--abort"); err == nil {
		return cause
	}
	if _, err := gitOut(ctx, root, "reset", "--hard", "HEAD"); err != nil {
		return fmt.Errorf("%w; and the tree could not be restored: %v", cause, err)
	}
	return cause
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

// gitStdout runs git and returns its standard output alone, byte for byte.
// gitOut merges stderr into the result, which makes a good error message and a
// corrupt patch; a diff is data and goes through here.
func gitStdout(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		text := strings.TrimSpace(stderr.String())
		if text == "" {
			text = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), text)
	}
	return string(out), nil
}

// mcpPathsOf resolves MCP config files against the spec's directory and
// checks them, so a typo fails before an agent runs. A list that is present
// but empty means no servers at all, not even ~/.hrn/mcp.json; a missing list
// gets that default like every other entry point.
func mcpPathsOf(dir string, paths []string) ([]string, error) {
	if paths != nil && len(paths) == 0 {
		return []string{}, nil
	}
	abs := make([]string, 0, len(paths))
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		abs = append(abs, p)
	}
	return mcp.ConfigPaths(abs)
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstList(lists ...[]string) []string {
	for _, l := range lists {
		if l != nil {
			return l
		}
	}
	return nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
