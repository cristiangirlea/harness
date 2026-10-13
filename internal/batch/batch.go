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
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/limits"
	"github.com/cristiangirlea/harness/internal/mcp"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/tools"
	"github.com/cristiangirlea/harness/internal/ws"
)

// Spec is a batch of tasks to run against one repository. Task fields
// override the Spec defaults of the same name.
type Spec struct {
	Dir      string `json:"dir,omitempty"`      // any path inside the repository; default: the spec file's directory
	Parallel int    `json:"parallel,omitempty"` // agents at once; default DefaultParallel
	Backend  string `json:"backend,omitempty"`  // default: api when credentials exist, else claude
	// Options are the run options every task starts from: model, mode (default
	// auto; nothing can approve prompts in a batch), the budgets (per attempt),
	// MCP files (relative to Dir) and the api switches. A task's own override
	// them field by field.
	agent.Options
	AllowDirty bool     `json:"allow_dirty,omitempty"` // start even though tracked files are modified
	Gate       []string `json:"gate,omitempty"`        // commands that must exit 0 in the worktree after the agent finishes
	// GateTimeout bounds each gate command; default 15m. A task's own wins.
	GateTimeout agent.Duration `json:"gate_timeout,omitempty"`
	Retry       []string       `json:"retry,omitempty"` // backends to try, in order, after a failed attempt
	// Worktrees is where each task works: WorktreesSlots (the default) or
	// WorktreesFresh.
	Worktrees string `json:"worktrees,omitempty"`
	Tasks     []Task `json:"tasks"`
}

const (
	// WorktreesSlots runs each task in a reusable slot of internal/ws, leased
	// while it runs, so the build output of earlier tasks is there.
	WorktreesSlots = "slots"
	// WorktreesFresh makes .hrn/worktrees/<id> per task, kept until hrn batch rm.
	WorktreesFresh = "fresh"
)

// DefaultParallel is how many agents run at once when a spec does not say.
// Four: the owner's PC has 32 threads, and an agent spends most of its time
// waiting on its model; a gate that builds already takes every core, so more
// than a few at once mostly compete for memory and disk.
const DefaultParallel = 4

type Task struct {
	ID      string `json:"id,omitempty"` // branch hrn/<id> (and worktree .hrn/worktrees/<id> when fresh); default t1, t2, ...
	Prompt  string `json:"prompt"`
	Backend string `json:"backend,omitempty"`
	// Options set here override the spec's, field by field; a switch can be
	// turned on for one task but not back off, and a zero limit inherits.
	agent.Options
	Gate        []string       `json:"gate,omitempty"`
	GateTimeout agent.Duration `json:"gate_timeout,omitempty"`
	Retry       []string       `json:"retry,omitempty"`
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
// first task starts, again each time a task moves on, and when the batch ends.
type Result struct {
	ID       string        `json:"id"`
	Started  time.Time     `json:"started"`
	Dir      string        `json:"dir"`  // repository root
	Base     string        `json:"base"` // commit every worktree branched from
	Parallel int           `json:"parallel"`
	Tasks    []TaskResult  `json:"tasks"`
	Duration time.Duration `json:"duration_ns"`
	// Ownership is how the record says which worktrees and branches are its
	// tasks' own. Run sets ownedByClaim: a slotless task owns them only when
	// its branch_made says so. Records from before it (0) never said so for a
	// fresh task, and are read as hrn wrote them then (owned, by path).
	Ownership int `json:"ownership,omitempty"`
}

// ownedByClaim is the Ownership of every record Run writes now.
const ownedByClaim = 1

type TaskResult struct {
	ID       string `json:"id"`
	Prompt   string `json:"prompt"`
	Status   Status `json:"status"`
	Branch   string `json:"branch"`
	Worktree string `json:"worktree"`
	Slot     int    `json:"slot,omitempty"` // the ws slot Worktree is; 0 for a fresh worktree
	// BranchMade says this task made Branch (takeSlot saves it before it
	// waits for a slot; a fresh task, with its worktree, once git made
	// both): a slotless task owns its branch only with it.
	BranchMade bool          `json:"branch_made,omitempty"`
	Commit     string        `json:"commit,omitempty"` // the agent's changes; empty when it changed nothing
	Attempts   []Attempt     `json:"attempts"`
	Error      string        `json:"error,omitempty"`
	Duration   time.Duration `json:"duration_ns"`
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
	// SwitchedFrom is the backend this attempt was meant for, or whose
	// limited attempt it carries on, when a usage limit moved it here.
	SwitchedFrom string `json:"switched_from,omitempty"`
}

type GateResult struct {
	Command  string        `json:"command"`
	ExitCode int           `json:"exit_code"` // -1 when killed (timeout or cancellation)
	Output   string        `json:"output"`    // last part of combined stdout/stderr
	Duration time.Duration `json:"duration_ns"`
}

// Event is progress for a caller to print. Kind is one of start, tool, run,
// gate, retry, switch, wait (for a free slot), done. Run delivers events one at a time, so the callback need
// not lock.
type Event struct {
	Task string
	Kind string
	Text string
}

const defaultGateTimeout = 15 * time.Minute

// maxGateTimeout is where a gate stops being a check and becomes a job.
const maxGateTimeout = 24 * time.Hour

// worktreeMu serialises `git worktree add`, `remove` and `prune` for this
// process. They all rewrite the repository's shared worktree metadata, and git
// takes no lock of its own around it, so two workers doing it at once can fail
// one another's command. Everything else the workers run - the agent, the
// gates, the commit - stays parallel, which is where the time goes.
var worktreeMu sync.Mutex

// slotPoll is how often a task waiting for a free slot looks again.
var slotPoll = 5 * time.Second

// unreadableWait is how long a task waits on a lease it cannot read before
// it gives up on it: one being written reads within a moment, one still
// unreadable after this long is a leftover no process will free.
var unreadableWait = 30 * time.Second

// sessionRetries is how many times a take is tried again at once when a
// session took a slot in its range after the range was worked out.
const sessionRetries = 3

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
		s.Parallel = DefaultParallel
	}
	if s.Mode == "" {
		s.Mode = agent.ModeAuto
	}
	switch s.Worktrees {
	case "":
		s.Worktrees = WorktreesSlots
	case WorktreesSlots, WorktreesFresh:
	default:
		return fmt.Errorf("worktrees %q: use %q (the default) or %q", s.Worktrees, WorktreesSlots, WorktreesFresh)
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
	if err := checkGateTimeout(s.GateTimeout); err != nil {
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
		if err := checkGateTimeout(t.GateTimeout); err != nil {
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

func checkGateTimeout(d agent.Duration) error {
	if d < 0 || time.Duration(d) > maxGateTimeout {
		return fmt.Errorf("gate_timeout %s: must be positive and at most %s (leave it out for the 15m default)", d, maxGateTimeout)
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
	// Keep the worktrees out of the repository's status. A slot task's
	// worktree is named once it has a slot, and ws keeps its own .gitignore.
	wtRoot := filepath.Join(root, ".hrn", "worktrees")
	fresh := spec.Worktrees == WorktreesFresh
	if fresh {
		if err := os.MkdirAll(wtRoot, 0o755); err != nil {
			return Result{}, err
		}
		if _, err := os.Stat(filepath.Join(root, ".hrn", ".gitignore")); err != nil {
			_ = os.WriteFile(filepath.Join(root, ".hrn", ".gitignore"), []byte("*\n"), 0o644)
		}
	}

	res := Result{ID: runlog.NewID(), Started: started, Dir: root, Base: base, Parallel: spec.Parallel, Tasks: make([]TaskResult, len(spec.Tasks)), Ownership: ownedByClaim}
	for i, t := range spec.Tasks {
		res.Tasks[i] = TaskResult{ID: t.ID, Prompt: t.Prompt, Status: StatusRunning, Branch: "hrn/" + t.ID, Attempts: []Attempt{}}
		if fresh {
			res.Tasks[i].Worktree = filepath.Join(wtRoot, t.ID)
		}
	}
	if err := save(res); err != nil {
		return Result{}, fmt.Errorf("saving batch record: %w", err)
	}

	// The record is saved again each time a task moves on (a run starts or
	// ends, a gate finishes, the task ends), so a reader such as hrn watch
	// sees the batch as it goes. A save that fails here only leaves that view
	// stale; the save at the end reports its error.
	var recordMu sync.Mutex
	update := func(i int, tr TaskResult) error {
		recordMu.Lock()
		defer recordMu.Unlock()
		res.Tasks[i] = tr
		return save(res)
	}

	kept := &keptSlots{}
	idx := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < spec.Parallel; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range idx {
				update(i, runTask(ctx, &spec, res.ID, kept, spec.Tasks[i], res.Tasks[i], root, base, notify,
					func(tr TaskResult) error { return update(i, tr) }))
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
	// gateTimeout bounds each gate command of this attempt.
	gateTimeout time.Duration
	// switchedFrom is the backend a usage limit moved this attempt from;
	// continues is the run of the limited attempt it carries on, if any.
	switchedFrom, continues string
}

// The prompt a retry gets. A gate failure is recoverable in place, so the next
// attempt is told what failed and that the work is already there, and so is
// an attempt the backend's usage limit cut short; anything else starts over
// and is told only how the previous attempt ended.
const (
	limitContext = "\n\n---\n" +
		"A previous attempt at this task (attempt %d on %s) stopped before it finished: %[2]s\n" +
		"reached its plan's usage limit. Its changes so far are committed on the current branch\n" +
		"in this workspace. Carry on from where it stopped rather than starting over.\n"
	limitLastMessage   = "\nIts last message:\n%s\n%s\n%s\n"
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
	switch g := failedGate(last); {
	case last.Result.Status == agent.StatusOK && g != nil:
		f := fence(g.Output)
		text = fmt.Sprintf(gateFailureContext, len(attempts), last.Backend, g.Command, g.ExitCode, f, g.Output, f)
		continued = true
	case last.Result.Status == agent.StatusLimited:
		text = fmt.Sprintf(limitContext, len(attempts), last.Backend)
		if final := strings.TrimSpace(last.Result.Final); final != "" {
			f := fence(final)
			text += fmt.Sprintf(limitLastMessage, f, tail(final, 4000), f)
		}
		continued = true
	default:
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

func runTask(ctx context.Context, spec *Spec, batchID string, kept *keptSlots, t Task, tr TaskResult, root, base string, notify func(Event), progress func(TaskResult) error) TaskResult {
	started := time.Now()
	tr.Status, tr.Attempts, tr.Commit, tr.Error, tr.BranchMade = "", []Attempt{}, "", "", false
	slots := spec.Worktrees == WorktreesSlots
	var slot *ws.Taken // the slot this task holds, given back when it ends
	finish := func(status Status, text string) TaskResult {
		if slot != nil {
			if err := giveBack(*slot, base); err != nil {
				kept.add(slot.Lease.Holder)
				msg := fmt.Sprintf("slot %d kept: %v", slot.N, err)
				tr.Error = strings.TrimSpace(tr.Error + "\n" + msg)
				text += " · " + msg
			}
			slot = nil
		}
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
	gateTimeout := defaultGateTimeout
	if d := firstNonZero(t.GateTimeout, spec.GateTimeout); d > 0 {
		gateTimeout = time.Duration(d)
	}
	// The spec's options with whatever the task sets on top; Run resolved
	// both MCP lists already.
	opts := t.Options.Over(spec.Options)

	if _, err := gitOut(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+tr.Branch); err == nil {
		return fail(errors.New(takenError("branch", tr.Branch)))
	}
	if !slots {
		if _, err := os.Stat(tr.Worktree); err == nil {
			return fail(errors.New(takenError("worktree", tr.Worktree)))
		}
	}

	task := agent.Task{Options: opts} // Dir is the worktree, once there is one
	expect, err := compileExpect(t.Expect)
	if err != nil {
		return fail(err)
	}

	// Backends whose usage limit this task reached: they do not start again
	// here, even when the limit gave no time to remember.
	spent := limits.Spent{}
	var outOfPlans string // why an attempt was skipped: claude and codex both limited
	var carry *Attempt    // the limited attempt the next one carries on
	for i := 0; i < len(backends); i++ {
		name := backends[i]
		plan := attemptPlan{number: len(tr.Attempts) + 1, prompt: t.Prompt, gateTimeout: gateTimeout}
		if carry != nil {
			plan.switchedFrom, plan.continues = carry.Backend, carry.RunID
			carry = nil
		}
		if !opts.NoSwitch {
			pick, why, bothOut := limits.Pick(name, time.Now(), spent)
			if bothOut {
				outOfPlans = why
				notify(Event{Task: t.ID, Kind: "switch", Text: "skipping an attempt on " + name + ": " + why})
				continue
			}
			if pick != name {
				notify(Event{Task: t.ID, Kind: "switch", Text: why})
				plan.switchedFrom, name = name, pick
			}
		}
		b, err := agent.Get(name)
		if err != nil {
			return fail(err)
		}
		if len(tr.Attempts) > 0 {
			text, continued := retryContext(tr.Attempts)
			plan.prompt, plan.continued = t.Prompt+text, continued
			where := "fresh worktree"
			if slots {
				where = "slot reset to the base"
			}
			if continued {
				where = "continuing in worktree"
			}
			verb := "retrying on " + name
			if plan.continues != "" {
				verb = fmt.Sprintf("%s reached its usage limit; carrying on on %s", plan.switchedFrom, name)
			}
			notify(Event{Task: t.ID, Kind: "retry", Text: fmt.Sprintf("%s (%s)", verb, where)})
		}
		plan.commit = t.ID + ": " + tools.OneLine(t.Prompt, 60)
		if plan.continued {
			plan.commit += fmt.Sprintf(" (attempt %d)", plan.number)
		}
		if !plan.continued {
			switch {
			case slots && slot != nil:
				// A fresh retry in the same slot: the last attempt left nothing
				// uncommitted, and its commit leaves the branch; ignored build output stays.
				if _, err := gitOut(ctx, slot.Path, "switch", "-q", "-C", tr.Branch, base); err != nil {
					return fail(err)
				}
			case slots:
				made := func(mine bool) error {
					cur := tr
					cur.BranchMade, cur.Status = mine, StatusRunning
					err := progress(cur) // whether the branch is this task's from now on
					if err == nil || mine {
						tr.BranchMade = mine
					}
					return err
				}
				got, err := takeSlot(ctx, spec, batchID, kept, t.ID, tr.Branch, root, base, notify, made)
				if err != nil {
					if tr.BranchMade {
						tr.Error = err.Error() // the branch is left: say so even when canceled
					}
					return fail(err)
				}
				slot = &got
				tr.Worktree, tr.Slot = got.Path, got.N
				cur := tr
				cur.Status = StatusRunning
				progress(cur) // the record names the slot from now on
			default:
				if len(tr.Attempts) > 0 {
					// The record stops claiming the worktree before it goes:
					// another batch could take the path before it is back.
					cur := tr
					cur.BranchMade, cur.Status = false, StatusRunning
					if err := progress(cur); err != nil {
						return fail(err) // the worktree stays, and is still claimed
					}
					tr.BranchMade = false
					if err := removeWorktree(root, tr); err != nil {
						tr.BranchMade, _ = stillMade(root, tr)
						return fail(err)
					}
				}
				if err := addWorktree(ctx, root, tr, base); err != nil {
					return fail(err)
				}
				cur := tr
				cur.BranchMade, cur.Status = true, StatusRunning
				if err := progress(cur); err != nil {
					// Unclaimed on disk, hrn batch rm would leave the worktree
					// and prune could drop the record: it goes now.
					err = fmt.Errorf("saving the claim on worktree %s: %w", tr.Worktree, err)
					if rerr := removeWorktree(root, tr); rerr != nil {
						var left string
						tr.BranchMade, left = stillMade(root, tr) // the last save may still record it
						return fail(fmt.Errorf("%w; %s: %v", err, left, rerr))
					}
					return fail(err)
				}
				tr.BranchMade = true // the worktree and branch are this task's from now on
			}
			tr.Commit = ""
		}
		task.Dir = tr.Worktree
		notify(Event{Task: t.ID, Kind: "start", Text: fmt.Sprintf("%s in %s", name, tr.Worktree)})

		// The attempt so far, on the task as it stands. Copies, because the
		// record is written while this worker goes on appending.
		report := func(att Attempt) {
			cur := tr
			cur.Status = StatusRunning
			att.Gates = slices.Clone(att.Gates)
			cur.Attempts = append(slices.Clone(tr.Attempts), att)
			progress(cur)
		}
		at := task
		if at.Lean == nil && agent.AppliesLean(b) {
			at.Lean = agent.Bool(true) // lean by default in a batch (#204)
		}
		out, err := runAttempt(ctx, b, t.ID, at, plan, expect, gates, notify, report)
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
		if att := out.att; att.Result.Status == agent.StatusLimited {
			spent[att.Backend] = true
			if err := limits.Note(att.Backend, att.Result); err != nil {
				notify(Event{Task: t.ID, Kind: "switch", Text: "the limit was not remembered: " + err.Error()})
			}
			// The partner carries the attempt on in this worktree, as one
			// more attempt that does not use up a retry.
			if other := limits.Partner(att.Backend); other != "" && !opts.NoSwitch {
				if ok, why := limits.Ready(other, time.Now(), spent); ok {
					backends = slices.Insert(backends, i+1, other)
					carry = &att
				} else {
					notify(Event{Task: t.ID, Kind: "switch", Text: "not carried on: " + why})
				}
			}
		}
	}
	if len(tr.Attempts) == 0 && outOfPlans != "" {
		tr.Error = outOfPlans
		return finish(StatusFail, "not started: "+outOfPlans)
	}
	left := "worktree kept at " + tr.Worktree
	if slots {
		left = "branch " + tr.Branch + " kept"
	}
	return finish(StatusFail, fmt.Sprintf("fail after %d attempt(s) · %s", len(tr.Attempts), left))
}

// takeSlot leases a slot of the repository for a task. The branch is made
// from the base first, so it starts there whatever origin has by that name.
// The batch holds at most max(parallel, ws.DefaultMax) slots of its own at
// once, running tasks' and kept ones together. It tries the slot numbers
// from 1 that hold that many slots other than sessions' (hrn ws take, no
// pid: held until hrn ws free), worked out again at every try, so a session
// among the first slots is passed over and one further up changes nothing.
// A session that takes a slot in the range after it was worked out widens
// it at once, a few times at most.
// While a slot it needs is held by a live process, another batch's or one
// of this batch's running tasks, it waits, saying so whenever who holds
// them changes, and looks again every slotPoll until ctx ends. When none
// can come free (sessions, skipped slots and the slots this batch kept) it
// fails instead. A task that gets no slot leaves no branch. made(true) is
// called once the branch exists, before any wait, to save that the branch
// is the task's: hrn batch rm deletes a slotless task's branch only on that
// proof. made(false) saves that it is not before the branch is dropped, and
// when that save fails the branch is left. A crash in between either way
// leaves a branch nothing deletes, which is the safe way round: never
// someone else's branch.
func takeSlot(ctx context.Context, spec *Spec, batchID string, kept *keptSlots, id, branch, root, base string, notify func(Event), made func(mine bool) error) (ws.Taken, error) {
	if _, err := gitOut(ctx, root, "branch", branch, base); err != nil {
		return ws.Taken{}, err
	}
	_ = made(true) // unsaved, the branch is only left behind if hrn crashes
	drop := func(cause error) (ws.Taken, error) {
		if err := made(false); err != nil {
			return ws.Taken{}, fmt.Errorf("%w; branch %s is left: saving that it is no longer the task's failed: %v", cause, branch, err)
		}
		dropBranch(root, branch)
		return ws.Taken{}, cause
	}
	prefix := "batch " + batchID + " "
	pid := os.Getpid()
	mine := func(l ws.Lease) bool { return l.PID == pid && strings.HasPrefix(l.Holder, prefix) }
	bound := max(spec.Parallel, ws.DefaultMax)
	opt := ws.TakeOptions{Repo: root, Branch: branch, Base: base, Holder: prefix + "task " + id, PID: pid, NoFetch: true}
	// try takes a slot under worktreeMu, so this batch's tasks count their
	// slots and take one by one. While the batch holds its bound it returns
	// an atBoundError, which says so.
	try := func() (ws.Taken, error) {
		worktreeMu.Lock()
		defer worktreeMu.Unlock()
		own, err := ws.CountLeases(ctx, root, mine)
		if err != nil {
			return ws.Taken{}, err
		}
		if own >= bound {
			return ws.Taken{}, &atBoundError{held: own, kept: kept.count()}
		}
		opt.Max, err = ws.RangeBeyondSessions(ctx, root, bound)
		if err != nil {
			return ws.Taken{}, err
		}
		// A session can take a slot in the range after it was worked out
		// and before the take scans that slot. When the take then finds a
		// session among what it passed over, the range is worked out again
		// and, if it reaches further, tried again; at most sessionRetries
		// times, so sessions arriving one after another cannot keep it here.
		for retry := 0; ; retry++ {
			if rangeComputed != nil {
				rangeComputed(opt.Max)
			}
			got, err := ws.Take(ctx, opt)
			var full *ws.NoSlotError
			if !errors.As(err, &full) || !holdsASession(full) || retry == sessionRetries {
				return got, err
			}
			wider, rerr := ws.RangeBeyondSessions(ctx, root, bound)
			if rerr != nil {
				return ws.Taken{}, rerr
			}
			if wider <= opt.Max {
				return got, err
			}
			opt.Max = wider
		}
	}
	var said string
	unreadable := unreadableLeases{}
	for {
		got, err := try()
		if err == nil {
			return got, nil
		}
		var full *ws.NoSlotError
		var bounded *atBoundError
		var wait string
		switch {
		case errors.As(err, &bounded):
			if bounded.kept >= bounded.held {
				return drop(fmt.Errorf("%w: none will come free while it runs", err))
			}
			// No lease was looked at, so none was seen unreadable: a lease
			// found so again later is timed from that look.
			clear(unreadable)
			wait = "waiting for a slot this batch holds to come back: " + err.Error()
		case errors.As(err, &full):
			// Only a slot a live process holds, another batch's or a running
			// task's of this batch, can come free: sessions', skipped slots
			// and the ones this batch kept stay as they are. A lease that
			// could not be read is being written, so it is looked at again;
			// one that stays unreadable for unreadableWait is a leftover no
			// process will free, so when those are all that is left to wait
			// for, the task fails and names them.
			stuck := unreadable.note(full, time.Now())
			switch {
			case someoneWillFree(full, kept):
			case stuck:
				return drop(unreadableError(full))
			case len(full.Unreadable) == 0:
				return drop(fmt.Errorf("no slot is free and none will come free while this batch runs: %w", full))
			}
			wait = "waiting for a free slot: " + strings.Join(full.Passed, "; ")
		default:
			return drop(err)
		}
		if wait != said {
			notify(Event{Task: id, Kind: "wait", Text: wait})
			said = wait
		}
		select {
		case <-ctx.Done():
			return drop(ctx.Err())
		case <-time.After(slotPoll):
		}
	}
}

// atBoundError is takeSlot's try failing because the batch holds as many
// slots as it may: held of them, kept of those for good.
type atBoundError struct{ held, kept int }

func (e *atBoundError) Error() string {
	return fmt.Sprintf("this batch already holds %d slots, the most it may (%d kept)", e.held, e.kept)
}

// holdsASession reports whether a slot full passed over is a session's: a
// lease without a pid, which RangeBeyondSessions skips.
func holdsASession(full *ws.NoSlotError) bool {
	for _, l := range full.Holders {
		if l.PID == 0 {
			return true
		}
	}
	return false
}

// someoneWillFree reports whether a slot full passed over is held by a
// process that will give it back: one not a session's (no pid) and not one
// this batch kept, or one whose lease changed hands under the take, which
// a live process has. A lease it could not read does not count: whether
// one is still being written is unreadableLeases' call.
func someoneWillFree(full *ws.NoSlotError, kept *keptSlots) bool {
	if full.Held > len(full.Holders)+len(full.Unreadable) {
		return true
	}
	for _, l := range full.Holders {
		if l.PID != 0 && !kept.has(l.Holder) {
			return true
		}
	}
	return false
}

// unreadableLeases is when each slot a waiting task passed over for a lease
// it could not read was first found so, kept across the task's looks for
// as long as every look finds it so.
type unreadableLeases map[int]time.Time

// note records full's unreadable slots as of now, forgets those readable
// again, and reports whether there are any and every one has stayed
// unreadable for unreadableWait.
func (u unreadableLeases) note(full *ws.NoSlotError, now time.Time) bool {
	seen := make(map[int]bool, len(full.Unreadable))
	stuck := len(full.Unreadable) > 0
	for _, n := range full.Unreadable {
		seen[n] = true
		since, ok := u[n]
		if !ok {
			since = now
			u[n] = now
		}
		if now.Sub(since) < unreadableWait {
			stuck = false
		}
	}
	for n := range u {
		if !seen[n] {
			delete(u, n)
		}
	}
	return stuck
}

// unreadableError is a task's error when the leases full passed over that
// it could not read are all that keeps it from a slot and they stayed so
// for unreadableWait: it names each and how to clear it. Either file goes
// only while no hrn command runs on the repository at all: a live take
// creates its lease empty before writing it and has not entered the slot
// yet, and a live hrn ws free or takeover holds the .takeover file while
// the lease it guards may name a dead process.
func unreadableError(full *ws.NoSlotError) error {
	var lines []string
	for _, n := range full.Unreadable {
		prefix := "slot " + strconv.Itoa(n) + " "
		for _, line := range full.Passed {
			if strings.HasPrefix(line, prefix) {
				lines = append(lines, line)
			}
		}
	}
	return fmt.Errorf("no slot is free: these leases could not be read for %s and no process will free them:\n  %s\ncheck the slots with `hrn ws list`. A lease that stays unreadable, or a left .takeover file, may be removed only while no hrn command at all runs on this repository, no hrn ws and no hrn batch: a live take creates its lease empty before writing it, and a live hrn ws free or takeover holds the .takeover file. Check with Task Manager or `tasklist | findstr hrn` on Windows, `pgrep hrn` elsewhere",
		unreadableWait, strings.Join(lines, "\n  "))
}

// keptSlots is the holders of the slots a batch's tasks could not give
// back, which stay leased by this process until it exits.
type keptSlots struct {
	mu      sync.Mutex
	holders map[string]bool
}

func (k *keptSlots) add(holder string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.holders == nil {
		k.holders = map[string]bool{}
	}
	k.holders[holder] = true
}

func (k *keptSlots) has(holder string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.holders[holder]
}

func (k *keptSlots) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.holders)
}

// droppingBranch, rangeComputed, failSave and failWorktreeList are for
// tests: the first runs as dropBranch starts, the second once takeSlot has
// worked out the slot numbers a take will try and before it tries them, the
// others can make save or git worktree list fail.
var (
	droppingBranch   func()
	rangeComputed    func(max int)
	failSave         func(Result) error
	failWorktreeList func() error
)

// dropBranch deletes the branch takeSlot made when no slot checked it out.
func dropBranch(root, branch string) {
	if droppingBranch != nil {
		droppingBranch()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, _ = gitOut(ctx, root, "branch", "-D", branch)
}

// giveBack returns a task's slot when the task ends. ws.Release detaches a
// clean slot at the base, so that once hrn batch rm deletes the branch the
// slot does not sit on a commit no ref holds, which the next take would
// skip. A slot with work left in it is refused by ws.Release and stays
// leased: the lease names this process, so it goes stale when hrn exits,
// and the next take skips the slot rather than clean it.
func giveBack(slot ws.Taken, base string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return ws.Release(ctx, slot, base)
}

type attemptOutcome struct {
	att    Attempt
	commit string // empty when the agent changed nothing
	pass   bool   // the run finished and every gate exited 0
}

// runAttempt runs the agent in the task's worktree, then the gates, then
// commits whatever was left behind, pass or fail. A returned error aborts the
// whole task; a failed run or gate is reported in the outcome instead.
func runAttempt(ctx context.Context, b agent.Backend, id string, task agent.Task, plan attemptPlan, expect *regexp.Regexp, gates []string, notify func(Event), report func(Attempt)) (attemptOutcome, error) {
	task.Prompt = plan.prompt
	att := Attempt{Backend: b.Name(), Prompt: plan.prompt, Continued: plan.continued, SwitchedFrom: plan.switchedFrom}
	if plan.switchedFrom != "" {
		task.Model = "" // the model named for the other backend means nothing here
	}

	// A carry-on's record continues the limited run, so the chain shows.
	rec, err := runlog.CreateContinuing(b.Name(), task, plan.continues)
	if err != nil {
		return attemptOutcome{att: att}, fmt.Errorf("creating run record: %w", err)
	}
	att.RunID = rec.ID()
	task.RunDir = rec.Dir() // so a finished attempt can be continued
	report(att)
	att.Result = agent.Run(ctx, b, task, func(e agent.Event) {
		rec.Event(e)
		if e.Kind == agent.EventToolCall {
			notify(Event{Task: id, Kind: "tool", Text: strings.TrimSpace("→ " + e.Tool + " " + tools.OneLine(e.Text, 120))})
		}
	})
	if err := rec.Close(att.Result); err != nil {
		att.Error = runlog.CloseMessage(err)
	}
	text := fmt.Sprintf("%s · %d turns · run %s", att.Result.Status, att.Result.Turns, att.RunID)
	if att.Result.Error != "" {
		text += " · " + tools.OneLine(att.Result.Error, 160)
	}
	if att.Error != "" {
		text += " · " + tools.OneLine(att.Error, 160)
	}
	report(att)
	notify(Event{Task: id, Kind: "run", Text: text})

	pass := att.Result.Status == agent.StatusOK
	if pass && expect != nil {
		gr := checkExpect(expect, att.Result.Final)
		att.Gates = append(att.Gates, gr)
		report(att)
		if gr.ExitCode != 0 {
			pass = false
			notify(Event{Task: id, Kind: "gate", Text: "✗ " + gr.Command})
		} else {
			notify(Event{Task: id, Kind: "gate", Text: "✓ " + gr.Command})
		}
	}
	if pass {
		for _, g := range gates {
			gr := runGate(ctx, task.Dir, g, plan.gateTimeout)
			att.Gates = append(att.Gates, gr)
			report(att)
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

func runGate(ctx context.Context, dir, command string, timeout time.Duration) GateResult {
	started := time.Now()
	// A gate is the operator's own command: it keeps hrn's environment.
	res, err := tools.RunShell(ctx, dir, command, timeout, nil)
	gr := GateResult{Command: command, ExitCode: res.ExitCode, Output: tail(res.Output, 4000), Duration: time.Since(started)}
	switch {
	case err != nil:
		gr.ExitCode = -1
		gr.Output = strings.TrimSpace(gr.Output + "\n" + err.Error())
	case res.TimedOut:
		gr.Output = strings.TrimSpace(gr.Output + fmt.Sprintf("\n[timed out after %s]", timeout))
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
// not hold that work. A slot task's slot is not the batch's to remove: only
// its branch goes. The record and the run logs stay.
func Remove(res Result) error {
	var errs []error
	for _, tr := range res.Tasks {
		if _, err := removeTask(res, tr); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", tr.ID, err))
		}
	}
	return errors.Join(errs...)
}

// RemoveTasks applies batch rm's per-task rules to only the named tasks and
// returns the IDs whose worktree or branch was removed, even when others fail.
func RemoveTasks(res Result, ids []string) ([]string, error) {
	tasks := make([]TaskResult, 0, len(ids))
	for _, id := range ids {
		tr, err := res.Find(id)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, tr)
	}
	var errs []error
	var removed []string
	for _, tr := range tasks {
		didRemove, err := removeTask(res, tr)
		if didRemove {
			removed = append(removed, tr.ID)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", tr.ID, err))
		}
	}
	return removed, errors.Join(errs...)
}

func removeTask(res Result, tr TaskResult) (bool, error) {
	branchExists := func() bool {
		_, err := gitOut(context.Background(), res.Dir, "show-ref", "--verify", "--quiet", "refs/heads/"+tr.Branch)
		return err == nil
	}
	if branchOnly(tr) {
		hadBranch := branchExists()
		err := removeSlotBranch(res.Dir, tr)
		return hadBranch && !branchExists(), err
	}
	if tr.Worktree == "" || notMine(res, tr) {
		return false, nil
	}
	// Only a directory git still lists is a worktree whose status means
	// anything: a leftover one has no .git of its own, and a status taken
	// inside it would describe the repository around it.
	registered, err := isWorktree(res.Dir, tr.Worktree)
	if err != nil {
		return false, err
	}
	if registered {
		if dirty, err := hasChanges(tr.Worktree); err == nil && dirty {
			return false, fmt.Errorf("%s has uncommitted changes; commit or discard them first", tr.Worktree)
		}
	}
	hadBranch := branchExists()
	err = removeWorktree(res.Dir, tr)
	stillRegistered, checkErr := isWorktree(res.Dir, tr.Worktree)
	return (registered && checkErr == nil && !stillRegistered) || (hadBranch && !branchExists()), err
}

// branchOnly reports whether a task's branch is all hrn batch rm removes of
// it: a slot task, or one whose record was saved while it waited for a slot
// (takeSlot makes the branch first) and never got one, as when hrn crashed
// then. Without a slot, only BranchMade proves the branch is the task's: Run
// saves every task as running before any branch exists, and a branch there
// already (another batch's) is what fails it. Old records lack the field, so
// they never claim a branch.
func branchOnly(tr TaskResult) bool {
	return tr.Slot != 0 || (tr.Worktree == "" && tr.Status == StatusRunning && tr.BranchMade)
}

// notMine reports whether a fresh task's worktree and branch are not its own
// to remove: Run names the worktree in every fresh task's record before the
// task starts, so the path alone does not make it the task's, and another
// batch can hold it (the task found it taken, or gave it up for a retry and
// stopped before it had it back). Only BranchMade proves it is the task's.
// Records from before Ownership never set it for a fresh task, whose
// worktree was its own unless finding it taken ended the task (foundTaken).
func notMine(res Result, tr TaskResult) bool {
	if res.Ownership >= ownedByClaim {
		return !tr.BranchMade
	}
	return foundTaken(tr)
}

// foundTaken reports whether a fresh task in a record from before Ownership
// failed because its worktree or branch was there already: before any
// attempt, with the error runTask gives then. Other errors can say
// "already exists" too (a hook's output), after the task made its worktree.
func foundTaken(tr TaskResult) bool {
	return tr.Slot == 0 && !tr.BranchMade && len(tr.Attempts) == 0 &&
		(tr.Error == takenError("branch", tr.Branch) || tr.Error == takenError("worktree", tr.Worktree))
}

// takenError is runTask's error for a task whose branch or worktree is there
// already; foundTaken matches it in old records, so its text stays as it is.
func takenError(what, name string) string {
	return fmt.Sprintf("%s %s already exists; remove it or pick another task id", what, name)
}

// removeSlotBranch deletes a slot task's branch, unless a worktree has it
// checked out right now: a task still running, or someone working on it.
func removeSlotBranch(root string, tr TaskResult) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := gitOut(ctx, root, "worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	for _, block := range strings.Split(out, "\n\n") {
		var path, branch string
		for _, line := range strings.Split(block, "\n") {
			if p, ok := strings.CutPrefix(line, "worktree "); ok {
				path = p
			}
			if b, ok := strings.CutPrefix(line, "branch refs/heads/"); ok {
				branch = b
			}
		}
		if branch == tr.Branch {
			return fmt.Errorf("branch %s is checked out in %s (a task still running?), so it stays", tr.Branch, filepath.FromSlash(path))
		}
	}
	if _, err := gitOut(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+tr.Branch); err != nil {
		return nil // gone already
	}
	_, err = gitOut(ctx, root, "branch", "-D", tr.Branch)
	return err
}

// isWorktree reports whether git lists path as one of root's worktrees.
func isWorktree(root, path string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if failWorktreeList != nil {
		if err := failWorktreeList(); err != nil {
			return false, err
		}
	}
	out, err := gitOut(ctx, root, "worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	want := comparablePath(path)
	for _, line := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok && comparablePath(p) == want {
			return true, nil
		}
	}
	return false, nil
}

// comparablePath normalises a path for equality: git prints forward slashes
// and long names on Windows, where paths are also case-insensitive.
func comparablePath(p string) string {
	p = filepath.Clean(filepath.FromSlash(p))
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

// addWorktree creates the task's worktree and branch from the base commit.
func addWorktree(ctx context.Context, root string, tr TaskResult, base string) error {
	worktreeMu.Lock()
	defer worktreeMu.Unlock()
	_, err := gitOut(ctx, root, "worktree", "add", "-q", "-b", tr.Branch, tr.Worktree, base)
	return err
}

// stillMade says whether anything a fresh task made outlived a removeWorktree
// that failed: its worktree, still registered, or its branch. Another batch
// takes neither while it is there, so what is left is still the task's; a
// directory git no longer knows is no claim. When git cannot list worktrees,
// only the branch decides: a claim may miss what is left, but must never name
// a path that another batch could take once a stray directory is cleared.
// left names what is still there, for the error.
func stillMade(root string, tr TaskResult) (made bool, left string) {
	registered, lerr := isWorktree(root, tr.Worktree)
	registered = lerr == nil && registered
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, berr := gitOut(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+tr.Branch)
	var parts []string
	switch _, serr := os.Stat(tr.Worktree); {
	case registered:
		parts = append(parts, "worktree "+tr.Worktree)
	case lerr != nil && serr == nil:
		parts = append(parts, "worktree or directory "+tr.Worktree)
	case serr == nil:
		parts = append(parts, "directory "+tr.Worktree)
	}
	if berr == nil {
		parts = append(parts, "branch "+tr.Branch)
	}
	switch len(parts) {
	case 0:
		left = "nothing is left"
	case 1:
		left = parts[0] + " is left"
	default:
		left = strings.Join(parts, " and ") + " are left"
	}
	return registered || berr == nil, left
}

func removeWorktree(root string, tr TaskResult) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	worktreeMu.Lock()
	defer worktreeMu.Unlock()
	var leftover error
	registered, err := isWorktree(root, tr.Worktree)
	switch {
	case err != nil:
		return err
	case registered:
		if _, err := gitOut(ctx, root, "worktree", "remove", "--force", tr.Worktree); err != nil {
			return err
		}
	default:
		// Gone, or no longer a worktree to git: on Windows a file lock can leave
		// the directory behind after git has dropped it. Clear git's stale entry,
		// remove the directory if nothing is in it, and still delete the branch,
		// which is all the batch committed.
		_, _ = gitOut(ctx, root, "worktree", "prune")
		leftover = removeEmptyDir(tr.Worktree)
	}
	if _, err := gitOut(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+tr.Branch); err == nil {
		if _, err := gitOut(ctx, root, "branch", "-D", tr.Branch); err != nil {
			return errors.Join(leftover, err)
		}
	}
	return leftover
}

// removeEmptyDir removes dir if it is empty and reports it if it is not: files
// in a directory git no longer tracks are in no branch, so they are not deleted.
func removeEmptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case len(entries) > 0:
		what := fmt.Sprintf("%d entries", len(entries))
		if len(entries) == 1 {
			what = entries[0].Name()
		}
		return fmt.Errorf("%s is not a git worktree any more but still holds %s; look at it, then delete the directory", dir, what)
	}
	return os.Remove(dir)
}

func batchesDir() string { return filepath.Join(runlog.Home(), "batches") }

func save(res Result) error {
	if failSave != nil {
		if err := failSave(res); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(batchesDir(), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	// Written beside the record and renamed over it: `hrn batch show` reads
	// this file while the batch is still running, and half a file is not a
	// record. hrn watch reads it all the time, for every page and stream open
	// on it, and on Windows os.Rename (MoveFileEx) refuses to replace a file
	// that anyone has open. os.Root's Rename replaces it with POSIX semantics
	// (Windows 10 and later, falling back by itself where the file system
	// lacks them), which readRecord's FILE_SHARE_DELETE readers allow. The
	// retries cover any other reader for a moment.
	root, err := os.OpenRoot(batchesDir())
	if err != nil {
		return err
	}
	defer root.Close()
	name := res.ID + ".json"
	if err := root.WriteFile(name+".tmp", data, 0o644); err != nil {
		return err
	}
	for try := 1; ; try++ {
		err = root.Rename(name+".tmp", name)
		if err == nil {
			return nil
		}
		if try == 20 {
			_ = root.Remove(name + ".tmp")
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
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
// started from, or whatever is still uncommitted in its fresh worktree; a
// slot task without a commit has an empty diff. Untracked paths come back
// separately, because a diff cannot show them.
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
	if tr.Slot != 0 {
		// The slot went back when the task ended and another task may hold it
		// now, so what is in it says nothing about this one.
		return "", nil, nil
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
	data, err := readRecord(filepath.Join(batchesDir(), id+".json"))
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

func firstNonZero(ds ...agent.Duration) agent.Duration {
	for _, d := range ds {
		if d != 0 {
			return d
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

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
