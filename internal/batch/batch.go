// Package batch runs parallel tasks in isolated git worktrees with gates and retries.
package batch

import (
	"bytes"
	"context"
	"encoding/binary"
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
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/flow"
	"github.com/cristiangirlea/harness/internal/gitenv"
	"github.com/cristiangirlea/harness/internal/gitretry"
	"github.com/cristiangirlea/harness/internal/limits"
	"github.com/cristiangirlea/harness/internal/mcp"
	"github.com/cristiangirlea/harness/internal/owner"
	"github.com/cristiangirlea/harness/internal/purpose"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/tools"
	"github.com/cristiangirlea/harness/internal/ws"
)

// Spec defines batch defaults and task overrides.
type Spec struct {
	Dir      string `json:"dir,omitempty"`      // any path inside the repository; default: the spec file's directory
	Parallel int    `json:"parallel,omitempty"` // agents at once; default DefaultParallel
	Backend  string `json:"backend,omitempty"`  // default: api when credentials exist, else claude
	// Options use noninteractive approval, per-attempt budgets and Dir-relative MCP paths.
	agent.Options
	AllowDirty bool     `json:"allow_dirty,omitempty"` // start even though tracked files are modified
	Gate       []string `json:"gate,omitempty"`        // commands that must exit 0 in the worktree after the agent finishes
	// GateTimeout bounds each gate; a task override wins.
	GateTimeout agent.Duration `json:"gate_timeout,omitempty"`
	Retry       []string       `json:"retry,omitempty"` // backends to try, in order, after a failed attempt
	// Protect uses slash globs relative to the repository.
	Protect     []string `json:"protect,omitempty"`
	ProtectMode string   `json:"protect_mode,omitempty"`
	// Worktrees selects reusable slots or fresh task worktrees.
	Worktrees string `json:"worktrees,omitempty"`
	Tasks     []Task `json:"tasks"`
	// Base is the commit tasks branch from instead of HEAD; set by callers, never by a spec file.
	Base string `json:"-"`
}

const (
	// WorktreesSlots retains build output in leased slots.
	WorktreesSlots = "slots"
	// WorktreesFresh retains per-task worktrees until removal.
	WorktreesFresh = "fresh"
)

// DefaultParallel limits contention from gates that can each use every core.
const DefaultParallel = 4

type Task struct {
	ID      string `json:"id,omitempty"` // branch hrn/<id> (and worktree .hrn/worktrees/<id> when fresh); default t1, t2, ...
	Prompt  string `json:"prompt"`
	Purpose string `json:"purpose,omitempty"`
	Backend string `json:"backend,omitempty"`
	// Options inherit zero limits; task switches can only enable.
	agent.Options
	Gate        []string       `json:"gate,omitempty"`
	GateTimeout agent.Duration `json:"gate_timeout,omitempty"`
	Retry       []string       `json:"retry,omitempty"`
	Protect     []string       `json:"protect,omitempty"`
	ProtectMode string         `json:"protect_mode,omitempty"`
	// Expect is a regexp the agent's final answer must match, checked as a gate before the others.
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

// Result is a saved batch record.
type Result struct {
	ID string `json:"id"`
	// Owner is empty on records written before ownership; owner.Of reads that as the local user.
	Owner    string        `json:"owner,omitempty"`
	Started  time.Time     `json:"started"`
	Dir      string        `json:"dir"`  // repository root
	Base     string        `json:"base"` // commit every worktree branched from
	Parallel int           `json:"parallel"`
	Tasks    []TaskResult  `json:"tasks"`
	Duration time.Duration `json:"duration_ns"`
	// Ownership selects BranchMade claims or legacy path ownership.
	Ownership int `json:"ownership,omitempty"`
}

// OwnershipClaim requires explicit slotless branch claims.
const OwnershipClaim = 1

type TaskResult struct {
	ID       string `json:"id"`
	Prompt   string `json:"prompt"`
	Purpose  string `json:"purpose,omitempty"`
	Status   Status `json:"status"`
	Branch   string `json:"branch"`
	Worktree string `json:"worktree"`
	Slot     int    `json:"slot,omitempty"` // the ws slot Worktree is; 0 for a fresh worktree
	// BranchMade proves ownership; save it before waiting for a slot.
	BranchMade bool          `json:"branch_made,omitempty"`
	Commit     string        `json:"commit,omitempty"` // the agent's changes; empty when it changed nothing
	Attempts   []Attempt     `json:"attempts"`
	Error      string        `json:"error,omitempty"`
	Duration   time.Duration `json:"duration_ns"`
}

type Attempt struct {
	Backend     string       `json:"backend"`
	RunID       string       `json:"run_id,omitempty"`
	Result      agent.Result `json:"result"`
	Gates       []GateResult `json:"gates,omitempty"`
	Error       string       `json:"error,omitempty"`
	DidNotStart string       `json:"did_not_start,omitempty"`
	// Prompt includes prior failures; Continued preserves work.
	Prompt    string `json:"prompt,omitempty"`
	Continued bool   `json:"continued,omitempty"`
	// SwitchedFrom identifies the backend replaced after a usage limit.
	SwitchedFrom string `json:"switched_from,omitempty"`
	// Protected lists changed or removed protected files against the base.
	Protected []string `json:"protected,omitempty"`
	// AgentCommitted says the agent moved its branch or HEAD with git itself (a commit, reset or rename).
	AgentCommitted bool `json:"agent_committed,omitempty"`
}

// AgentCommitted reports whether any attempt's agent committed on its own.
func (t TaskResult) AgentCommitted() bool {
	return slices.ContainsFunc(t.Attempts, func(a Attempt) bool { return a.AgentCommitted })
}

type GateResult struct {
	Command  string        `json:"command"`
	ExitCode int           `json:"exit_code"` // -1 when killed: Killed says why
	Output   string        `json:"output"`    // last part of combined stdout/stderr
	Duration time.Duration `json:"duration_ns"`
	// Killed says why an exit -1 gate never finished (KilledTimeout or KilledCanceled); empty in older records.
	Killed  string         `json:"killed,omitempty"`
	Timeout agent.Duration `json:"timeout,omitempty"`
	// InfrastructureError is a gate shell that could not start or be waited for: the operator's to fix, not a retry backend's.
	InfrastructureError string `json:"infrastructure_error,omitempty"`
	// Guard marks the protect check, so a user gate whose command starts the same way is not taken for it.
	Guard bool `json:"guard,omitempty"`
}

const (
	KilledTimeout  = "timeout"
	KilledCanceled = "canceled"
)

// Outcome describes gate failure with operator advice.
func (g GateResult) Outcome() string {
	if g.InfrastructureError != "" {
		return "infrastructure failure: " + g.InfrastructureError
	}
	switch g.Killed {
	case KilledTimeout:
		return fmt.Sprintf("killed after %s (gate_timeout; set %q in the spec to allow more)", g.Timeout, "gate_timeout")
	case KilledCanceled:
		return "canceled"
	}
	return fmt.Sprintf("exited %d", g.ExitCode)
}

// Brief omits operator-only advice from retry prompts.
func (g GateResult) Brief() string {
	switch g.Killed {
	case KilledTimeout:
		return fmt.Sprintf("was killed after %s by gate_timeout", g.Timeout)
	case KilledCanceled:
		return "was canceled"
	}
	return fmt.Sprintf("exited %d", g.ExitCode)
}

// Event is progress for a caller; Kind is start, tool, run, gate, retry, switch, wait or done.
type Event struct {
	Task string
	Kind string
	Text string
}

const defaultGateTimeout = 15 * time.Minute

const maxGateTimeout = 24 * time.Hour

// Refresh records so silent gates do not appear stopped.
var gateBeat = 10 * time.Minute

// Git leaves shared worktree metadata unlocked; serialize its mutations.
var worktreeMu sync.Mutex

var slotPoll = 5 * time.Second

// Allow lease writes to finish; bound waits on unreadable leftovers.
var unreadableWait = 30 * time.Second

// Bound retries against session starvation.
const sessionRetries = 3

var idRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// LoadSpec resolves Dir relative to the spec file.
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

// Validate before spending agent time.
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
	if err := checkBackends(s.Backend, &s.Retry); err != nil {
		return err
	}
	if err := checkGates(s.Gate); err != nil {
		return err
	}
	if err := checkGateTimeout(s.GateTimeout); err != nil {
		return err
	}
	if err := checkProtect(s.Protect, s.ProtectMode); err != nil {
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
		if t.Purpose != "" && !purpose.Valid(t.Purpose) {
			return fmt.Errorf("task %s: purpose %q: one of %s", t.ID, t.Purpose, strings.Join(purpose.Declarable(), ", "))
		}
		if t.Purpose == "" {
			t.Purpose = purpose.Declared(t.Prompt)
		}
		if err := t.Options.Over(s.Options).Validate(); err != nil {
			return fmt.Errorf("task %s: %w", t.ID, err)
		}
		if err := checkBackends(t.Backend, &t.Retry); err != nil {
			return fmt.Errorf("task %s: %w", t.ID, err)
		}
		if err := checkRetrievalRetry(t.Options.Over(s.Options), firstOf(t.Backend, s.Backend), firstList(t.Retry, s.Retry)); err != nil {
			return fmt.Errorf("task %s: %w", t.ID, err)
		}
		if err := checkGates(t.Gate); err != nil {
			return fmt.Errorf("task %s: %w", t.ID, err)
		}
		// Empty gate lists lose their distinction from inherited gates when records omit them.
		if t.Gate != nil && len(t.Gate) == 0 {
			return fmt.Errorf("task %s: empty gate list: leave gate out to use the spec's gates, or give each task that needs gates its own", t.ID)
		}
		if err := checkGateTimeout(t.GateTimeout); err != nil {
			return fmt.Errorf("task %s: %w", t.ID, err)
		}
		if err := checkProtect(t.Protect, t.ProtectMode); err != nil {
			return fmt.Errorf("task %s: %w", t.ID, err)
		}
		if _, err := compileExpect(t.Expect); err != nil {
			return fmt.Errorf("task %s: %w", t.ID, err)
		}
	}
	return nil
}

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

func expectCommand(expr string) string { return "expect /" + expr + "/" }

// Retrieval pins are backend-scoped, so retries cannot move them to another backend.
func checkRetrievalRetry(opts agent.Options, backend string, retry []string) error {
	if !retrievalOn(opts) {
		return nil
	}
	backend = firstOf(backend, agent.DefaultBackendNameIn(runlog.Home()))
	for _, name := range retry {
		if name != backend {
			return fmt.Errorf("retry names %s, but retrieval pins the task to its backend %s: retry on %s only, or turn retrieval off", name, backend, backend)
		}
	}
	return nil
}

func retrievalOn(opts agent.Options) bool { return opts.Retrieval != "" && opts.Retrieval != "off" }

func checkBackends(backend string, retry *[]string) error {
	p, err := agent.BackendPreferences(runlog.Home())
	if err != nil {
		return err
	}
	if err := p.Check(backend); err != nil {
		return err
	}
	for _, name := range append([]string{backend}, (*retry)...) {
		if name == "" {
			continue
		}
		if _, err := agent.Get(name); err != nil {
			return err
		}
	}
	if len(*retry) > 0 {
		filtered := make([]string, 0, len(*retry))
		for _, name := range *retry {
			if p.Enabled(name) {
				filtered = append(filtered, name)
			}
		}
		if len(filtered) == 0 {
			return fmt.Errorf("retry has no enabled backends; enabled: %s (hrn backends set)", strings.Join(p.EnabledNames, ", "))
		}
		*retry = filtered
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

// Agents start at HEAD and cannot see tracked edits.
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

// Run executes and records a batch, stopping agents when ctx is canceled.
// Run delivers events one at a time, so the callback need not lock.
func Run(ctx context.Context, spec Spec, notify func(Event)) (Result, error) {
	started := time.Now()
	if err := spec.normalize(); err != nil {
		return Result{}, err
	}
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

	root, err := repoRoot(ctx, spec.Dir)
	if err != nil {
		return Result{}, err
	}
	base, err := gitOut(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return Result{}, err
	}
	if spec.Base != "" {
		if base, err = gitOut(ctx, root, "rev-parse", "--verify", "--quiet", spec.Base+"^{commit}"); err != nil {
			return Result{}, fmt.Errorf("base %s: %w", spec.Base, err)
		}
	}
	if !spec.AllowDirty {
		if err := checkClean(ctx, root); err != nil {
			return Result{}, err
		}
	}
	if spec.MCP, err = mcpPathsOf(spec.Dir, spec.MCP); err != nil {
		return Result{}, err
	}
	for i := range spec.Tasks {
		if t := &spec.Tasks[i]; t.MCP != nil {
			if t.MCP, err = mcpPathsOf(spec.Dir, t.MCP); err != nil {
				return Result{}, fmt.Errorf("task %s: %w", t.ID, err)
			}
		}
	}
	// Keep fresh worktrees out of status; ws manages slot ignores.
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

	res := Result{ID: runlog.NewID(), Owner: owner.Local(), Started: started, Dir: root, Base: base, Parallel: spec.Parallel, Tasks: make([]TaskResult, len(spec.Tasks)), Ownership: OwnershipClaim}
	for i, t := range spec.Tasks {
		res.Tasks[i] = TaskResult{ID: t.ID, Prompt: t.Prompt, Purpose: t.Purpose, Status: StatusRunning, Branch: "hrn/" + t.ID, Attempts: []Attempt{}}
		if fresh {
			res.Tasks[i].Worktree = filepath.Join(wtRoot, t.ID)
		}
	}
	if err := save(res); err != nil {
		return Result{}, fmt.Errorf("saving batch record: %w", err)
	}
	flowWriter, _ := flow.Open(FlowPath(res.ID), flow.Event{Owner: res.Owner, Batch: res.ID})
	defer func() { _ = flowWriter.Close() }()
	flowWriter.Emit(flow.Event{Step: BatchStep, Kind: flow.KindBatch, Status: flow.StatusStarted, Name: res.ID,
		Text: fmt.Sprintf("%d tasks, %d parallel, base %s", len(res.Tasks), res.Parallel, short(res.Base))})

	// Intermediate save failures stale the view; final save errors are returned.
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
				tr := runTask(ctx, &spec, res.ID, kept, spec.Tasks[i], res.Tasks[i], root, base, flowWriter, notify,
					func(tr TaskResult) error { return update(i, tr) })
				update(i, tr)
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
	flowWriter.Emit(flow.Event{Step: BatchStep, Kind: flow.KindBatch, Status: batchStatus(res), Text: tally(res)})
	return res, nil
}

type attemptPlan struct {
	routedBy *runlog.RoutedBy
	number   int    // 1-based
	prompt   string // what the agent is sent, failure context included
	commit   string // the commit subject for whatever it leaves behind
	base     string // the commit the worktree started from
	branch   string // the task's branch, which a merge takes
	// Keep work for in-place repair on retry.
	continued               bool
	gateTimeout             time.Duration
	switchedFrom, continues string
	// Model and effort are backend-scoped.
	chosenFor string
	reason    string
}

const (
	limitContext = "\n\n---\n" +
		"A previous attempt at this task (attempt %d on %s) stopped before it finished: %[2]s\n" +
		"reached its plan's usage limit. Its changes so far are committed on the current branch\n" +
		"in this workspace. Carry on from where it stopped rather than starting over.\n"
	limitLastMessage   = "\nIts last message:\n%s\n%s\n%s\n"
	gateFailureContext = "\n\n---\n" +
		"A previous attempt at this task (attempt %d on %s) did not pass its gate. Its changes\n" +
		"are already applied in this workspace and committed on the current branch. Build on\n" +
		"them rather than starting over.\n\n" +
		"The gate command `%s` %s. Output (tail):\n" +
		"%s\n%s\n%s\n"
	// Timeouts do not prove code failure; tell retries not to invent one.
	gateTimeoutContext = "\nThe gate did not fail: hrn killed it when its time limit of %s ran out, so its\n" +
		"output above is incomplete; a failure it shows from before the kill still needs fixing,\n" +
		"but do not search it for one that is not there. The command must finish within that\n" +
		"limit; make it faster, or leave it and say so in your answer, since only the operator\n" +
		"can raise \"gate_timeout\" in the batch spec.\n"
	runFailureContext = "\n\n---\n" +
		"A previous attempt on %s ended with %s: %s\n" +
		"This workspace is a fresh copy; nothing from that attempt is in it.\n"
	attemptHistory = "\nEarlier attempts, so they are not tried again: %s.\n"
)

func failedGate(att Attempt) *GateResult {
	for i := range att.Gates {
		if att.Gates[i].ExitCode != 0 {
			return &att.Gates[i]
		}
	}
	return nil
}

// Include all attempts to avoid repeating failed repairs.
func retryContext(attempts []Attempt) (text string, continued bool) {
	last := attempts[len(attempts)-1]
	switch g := failedGate(last); {
	case last.Result.Status == agent.StatusOK && g != nil:
		f := fence(g.Output)
		text = fmt.Sprintf(gateFailureContext, len(attempts), last.Backend, g.Command, g.Brief(), f, g.Output, f)
		if g.Killed == KilledTimeout {
			text += fmt.Sprintf(gateTimeoutContext, g.Timeout)
		}
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

func outcomeOf(att Attempt) string {
	if g := failedGate(att); g != nil {
		return fmt.Sprintf("(gate `%s` %s)", tools.OneLine(g.Command, 60), g.Brief())
	}
	if att.Result.Status != agent.StatusOK {
		return fmt.Sprintf("(%s: %s)", att.Result.Status, tools.OneLine(att.Result.Error, 80))
	}
	return "(passed)"
}

// Output must not close its enclosing Markdown fence.
func fence(s string) string {
	f := "```"
	for strings.Contains(s, f) {
		f += "`"
	}
	return f
}

func runTask(ctx context.Context, spec *Spec, batchID string, kept *keptSlots, t Task, tr TaskResult, root, base string, fw *flow.Writer, notify func(Event), progress func(TaskResult) error) TaskResult {
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
		st := taskStatus(status)
		e := flow.Event{Step: TaskStep(t.ID), Kind: flow.KindTask, Task: t.ID, Status: st, Text: firstLineOf(tr.Error)}
		if st == flow.StatusOK && tr.Commit != "" {
			e.Next = "review the diff with hrn batch show " + batchID + " " + t.ID + ", then merge it"
		}
		fw.Emit(e)
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
		fw.Emit(flow.Event{Step: TaskStep(t.ID), Parent: BatchStep, Kind: flow.KindTask, Task: t.ID, Status: flow.StatusStarted,
			Name: t.ID, Text: promptLine(t.Prompt), Reason: purposeReason(t.Purpose)})
		return finish(StatusCanceled, "canceled")
	}
	fw.Emit(flow.Event{Step: TaskStep(t.ID), Parent: BatchStep, Kind: flow.KindTask, Task: t.ID, Status: flow.StatusStarted,
		Name: t.ID, Text: promptLine(t.Prompt), Reason: purposeReason(t.Purpose)})
	decisions := 0
	decide := func(st flow.Status, reason string) {
		decisions++
		fw.Emit(flow.Event{Step: fmt.Sprintf("%s/d%d", TaskStep(t.ID), decisions), Parent: TaskStep(t.ID), Kind: flow.KindDecision,
			Task: t.ID, Status: st, Reason: reason})
	}

	opts := t.Options.Over(spec.Options)
	backend := firstOf(t.Backend, spec.Backend)
	routedBy, routeNote, err := opts.RouteWithNote(t.Purpose, t.Prompt, &backend, runlog.Home())
	if err != nil {
		return fail(err)
	}
	routeReason := routeNote
	if backend == "" {
		routeReason = agent.WithDefault(routeNote, agent.DefaultBackendNameIn(runlog.Home()))
	}
	if routedBy != nil {
		mode := "classified"
		if routedBy.Declared {
			mode = "declared"
		}
		routeReason = fmt.Sprintf("route category %s, %s, set by %s", routedBy.Category, mode, routedBy.Set)
		if routedBy.Eval != "" {
			routeReason += ", from eval " + routedBy.Eval
		}
	}
	fw.Emit(flow.Event{Step: TaskStep(t.ID) + "/route", Parent: TaskStep(t.ID), Kind: flow.KindRoute, Task: t.ID,
		Status: flow.StatusOK, Name: firstOf(backend, agent.DefaultBackendNameIn(runlog.Home())), Reason: routeReason})
	backends := []string{firstOf(backend, agent.DefaultBackendNameIn(runlog.Home()))}
	p, err := agent.BackendPreferences(runlog.Home())
	if err != nil {
		return fail(err)
	}
	if err := p.Check(backends[0]); err != nil {
		return fail(err)
	}
	backends = append(backends, firstList(t.Retry, spec.Retry)...)
	gates := firstList(t.Gate, spec.Gate)
	guard := protection{globs: firstList(t.Protect, spec.Protect), fail: firstOf(t.ProtectMode, spec.ProtectMode) == ProtectFail, base: base}
	gateTimeout := defaultGateTimeout
	if d := firstNonZero(t.GateTimeout, spec.GateTimeout); d > 0 {
		gateTimeout = time.Duration(d)
	}
	if _, err := gitOut(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+tr.Branch); err == nil {
		return fail(errors.New(takenError("branch", tr.Branch)))
	}
	if !slots {
		if _, err := os.Stat(tr.Worktree); err == nil {
			return fail(errors.New(takenError("worktree", tr.Worktree)))
		}
	}

	task := agent.Task{Options: opts, OriginalPrompt: t.Prompt, Purpose: t.Purpose} // Dir is the worktree, once there is one
	expect, err := compileExpect(t.Expect)
	if err != nil {
		return fail(err)
	}

	// Skip exhausted backends even without a reset time.
	spent := limits.Spent{}
	var outOfPlans string // why an attempt was skipped: claude and codex both limited
	var carry *Attempt    // the limited attempt the next one carries on
	for i := 0; i < len(backends); i++ {
		name := backends[i]
		plan := attemptPlan{number: len(tr.Attempts) + 1, prompt: t.Prompt, gateTimeout: gateTimeout, chosenFor: backends[0], base: base, branch: tr.Branch}
		if i == 0 {
			plan.routedBy = routedBy
		}
		if carry != nil {
			plan.switchedFrom, plan.continues = carry.Backend, carry.RunID
			carry = nil
		}
		if !opts.NoSwitch {
			pick, why, bothOut := limits.Pick(name, time.Now(), spent)
			if bothOut {
				outOfPlans = why
				text := "skipping an attempt on " + name + ": " + why
				decide(flow.StatusSkipped, text)
				notify(Event{Task: t.ID, Kind: "switch", Text: text})
				continue
			}
			if pick != name && retrievalOn(opts) {
				bound := name
				if task.RetrievalState != nil && task.RetrievalState.Scope.Backend != "" {
					bound = task.RetrievalState.Scope.Backend
				}
				tr.Error = pinnedSwitch(name, pick, bound)
				decide(flow.StatusSkipped, tr.Error)
				return finish(StatusFail, "limited: "+tr.Error)
			}
			if pick != name {
				plan.switchedFrom, name = name, pick
				decide(flow.StatusOK, why)
				notify(Event{Task: t.ID, Kind: "switch", Text: why})
			} else if why != "" {
				decide(flow.StatusOK, why)
				notify(Event{Task: t.ID, Kind: "info", Text: why})
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
			text = fmt.Sprintf("%s (%s)", verb, where)
			decide(flow.StatusOK, text)
			notify(Event{Task: t.ID, Kind: "retry", Text: text})
		}
		plan.commit = t.ID + ": " + tools.OneLine(t.Prompt, 60)
		if plan.continued {
			plan.commit += fmt.Sprintf(" (attempt %d)", plan.number)
		}
		if !plan.continued {
			switch {
			case slots && slot != nil:
				// The last attempt left nothing uncommitted, so -C drops only its commit; ignored build output stays.
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
				got, err := takeSlotFlow(ctx, spec, batchID, kept, t.ID, tr.Branch, root, base, fw, notify, made)
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
					// Drop ownership before another batch can take the path.
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
					// Remove now: cleanup cannot recover an unrecorded claim.
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

		// Copy slices: workers append while records are saved.
		report := func(att Attempt) {
			cur := tr
			cur.Status = StatusRunning
			att.Gates = slices.Clone(att.Gates)
			cur.Attempts = append(slices.Clone(tr.Attempts), att)
			progress(cur)
		}
		at := task
		if at.Lean == nil && agent.AppliesLean(b) {
			at.Lean = agent.Bool(true) // lean by default in a batch
		}
		plan.number = len(tr.Attempts) + 1
		plan.reason = attemptReason(append(slices.Clone(tr.Attempts), Attempt{Backend: name, Continued: plan.continued, SwitchedFrom: plan.switchedFrom}), len(tr.Attempts))
		out, err := runAttempt(ctx, b, t.ID, at, plan, expect, guard, gates, fw, notify, report)
		tr.Attempts = append(tr.Attempts, out.att)
		task.RetrievalState = out.att.Result.Retrieval
		task.RetrievalRepair = false
		for _, g := range out.att.Gates {
			if g.ExitCode != 0 {
				task.RetrievalRepair = true
			}
		}
		// Agents can reset the branch; retain old commits only on failure or missing worktrees.
		if _, gone := os.Stat(task.Dir); err == nil && (out.commit != "" || gone == nil) {
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
		if g := failedGate(out.att); g != nil && g.InfrastructureError != "" {
			tr.Error = fmt.Sprintf("gate %q: %s", g.Command, g.Outcome())
			return finish(StatusFail, tr.Error)
		}
		if att := out.att; att.Result.Status == agent.StatusLimited {
			spent[att.Backend] = true
			if err := limits.Note(att.Backend, att.Result); err != nil {
				text := "the limit was not remembered: " + err.Error()
				decide(flow.StatusOK, text)
				notify(Event{Task: t.ID, Kind: "switch", Text: text})
			}
			// A usage-limit carry-on preserves work without consuming a retry.
			if other := limits.Partner(att.Backend, time.Now(), spent); other != "" && !opts.NoSwitch {
				if ok, why := limits.Ready(other, time.Now(), spent); ok && retrievalOn(opts) {
					tr.Error = pinnedSwitch(att.Backend, other, att.Backend)
					return finish(StatusFail, "limited: "+tr.Error)
				} else if ok {
					backends = slices.Insert(backends, i+1, other)
					carry = &att
				} else {
					text := "not carried on: " + why
					decide(flow.StatusOK, text)
					notify(Event{Task: t.ID, Kind: "switch", Text: text})
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

// Retrieval pins cannot follow a usage-limit switch to another backend.
func pinnedSwitch(from, to, bound string) string {
	return fmt.Sprintf("%s reached its usage limit; switch to %s skipped: the retrieval pin is bound to %s", from, to, bound)
}

func takeSlot(ctx context.Context, spec *Spec, batchID string, kept *keptSlots, id, branch, root, base string, notify func(Event), made func(mine bool) error) (ws.Taken, error) {
	return takeSlotFlow(ctx, spec, batchID, kept, id, branch, root, base, nil, notify, made)
}

// Save ownership before waiting; clear it before branch deletion.
// On save failure, leak rather than delete unproven work.
func takeSlotFlow(ctx context.Context, spec *Spec, batchID string, kept *keptSlots, id, branch, root, base string, fw *flow.Writer, notify func(Event), made func(mine bool) error) (_ ws.Taken, failed error) {
	step := SlotStep(id)
	fw.Emit(flow.Event{Step: step, Parent: TaskStep(id), Kind: flow.KindSlot, Task: id, Status: flow.StatusWaiting, Next: "waits for a free worktree slot"})
	defer func() {
		if failed == nil {
			return
		}
		st := flow.StatusFailed
		if ctx.Err() != nil {
			st = flow.StatusCanceled
		}
		fw.Emit(flow.Event{Step: step, Kind: flow.KindSlot, Task: id, Status: st, Text: firstLineOf(failed.Error())})
	}()
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
	bound := min(max(spec.Parallel, ws.DefaultMax), ws.Limit)
	opt := ws.TakeOptions{Repo: root, Branch: branch, Base: base, Holder: prefix + "task " + id, PID: pid, NoFetch: true}
	// Count and take under worktreeMu to enforce the slot bound.
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
		// Recompute after racing sessions; bound retries against starvation.
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
			fw.Emit(flow.Event{Step: step, Parent: TaskStep(id), Kind: flow.KindSlot, Task: id, Status: flow.StatusOK, Text: fmt.Sprintf("slot %d at %s", got.N, got.Path)})
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
			// No lease observed means no consecutive unreadable observation.
			clear(unreadable)
			wait = "waiting for a slot this batch holds to come back: " + err.Error()
		case errors.As(err, &full):
			// Only live processes or briefly unreadable leases may free slots.
			stuck := unreadable.note(full, time.Now())
			switch {
			case someoneWillFree(full, kept):
			case stuck:
				return drop(unreadableError(full))
			case len(full.Unreadable) == 0:
				return drop(fmt.Errorf("no slot is free and none will come free while this batch runs: %w", full))
			}
			wait = "waiting for a free slot: " + strings.Join(full.Passed, "; ")
			if full.Max == ws.Limit {
				wait += fmt.Sprintf("; at most %d slots per repository", ws.Limit)
			}
		default:
			return drop(err)
		}
		if wait != said {
			fw.Emit(flow.Event{Step: step, Parent: TaskStep(id), Kind: flow.KindSlot, Task: id, Status: flow.StatusWaiting, Text: wait})
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

type atBoundError struct{ held, kept int }

func (e *atBoundError) Error() string {
	return fmt.Sprintf("this batch already holds %d slots, the most it may (%d kept)", e.held, e.kept)
}

func holdsASession(full *ws.NoSlotError) bool {
	for _, l := range full.Holders {
		if l.PID == 0 {
			return true
		}
	}
	return false
}

// Sessions never free themselves; unreadable leases need a grace period.
func someoneWillFree(full *ws.NoSlotError, kept *keptSlots) bool {
	// More held than listed: a lease changed hands mid-take, which only a live process does.
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

type unreadableLeases map[int]time.Time

// Only consecutive unreadable observations prove a stuck lease.
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

// Remove lease files only with hrn stopped: live takes start empty and takeovers guard dead pids.
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

// Keep failed releases leased so other tasks cannot clean their work.
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

var (
	droppingBranch   func()
	rangeComputed    func(max int)
	failSave         func(Result) error
	failWorktreeList func() error
	saveRename       = (*os.Root).Rename
	saveSleep        = time.Sleep
)

func dropBranch(root, branch string) {
	if droppingBranch != nil {
		droppingBranch()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, _ = gitOut(ctx, root, "branch", "-D", branch)
}

// Release at base so branch deletion cannot leave an unreferenced slot tip.
// Dirty slots stay leased until exit; the next take must skip their work.
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

// runAttempt's returned error aborts the whole task; a failed run or gate is reported in the outcome instead.
func runAttempt(ctx context.Context, b agent.Backend, id string, task agent.Task, plan attemptPlan, expect *regexp.Regexp, guard protection, gates []string, fw *flow.Writer, notify func(Event), report func(Attempt)) (attemptOutcome, error) {
	task.Prompt = plan.prompt
	att := Attempt{Backend: b.Name(), Prompt: plan.prompt, Continued: plan.continued, SwitchedFrom: plan.switchedFrom}
	if plan.switchedFrom != "" {
		task.Model = "" // the model named for the other backend means nothing here
	}
	if b.Name() != plan.chosenFor {
		task.Model, task.Effort, plan.routedBy = "", "", nil
	}

	rec, err := runlog.CreateContinuing(b.Name(), task, plan.continues, plan.routedBy)
	if err != nil {
		return attemptOutcome{att: att}, fmt.Errorf("creating run record: %w", err)
	}
	att.RunID = rec.ID()
	as := AttemptStep(id, plan.number)
	fw.Emit(flow.Event{Step: as, Parent: TaskStep(id), Kind: flow.KindAttempt, Task: id, Attempt: plan.number,
		Status: flow.StatusStarted, Name: b.Name(), Reason: plan.reason, Run: att.RunID})
	task.RunDir = rec.Dir() // so a finished attempt can be continued
	report(att)
	before, beforeErr := gitTips(task.Dir, plan.branch)
	att.Result = agent.Run(ctx, b, task, func(e agent.Event) {
		rec.Event(e)
		if e.Kind == agent.EventToolCall {
			notify(Event{Task: id, Kind: "tool", Text: strings.TrimSpace("→ " + e.Tool + " " + tools.OneLine(e.Text, 120))})
		}
	})
	att.DidNotStart = startFailure(att)
	if after, err := gitTips(task.Dir, plan.branch); beforeErr == nil && err == nil && before != "" && after != before {
		att.AgentCommitted = true
	}
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
		fw.Emit(flow.Event{Step: GateStep(id, plan.number, len(att.Gates)+1), Parent: as, Kind: flow.KindGate, Task: id, Attempt: plan.number,
			Status: flow.StatusStarted, Name: expectCommand(expect.String())})
		gr := checkExpect(expect, att.Result.Final)
		att.Gates = append(att.Gates, gr)
		fw.Emit(gateEvent(id, plan.number, len(att.Gates), as, gr))
		report(att)
		if gr.ExitCode != 0 {
			pass = false
			notify(Event{Task: id, Kind: "gate", Text: "✗ " + gr.Command})
		} else {
			notify(Event{Task: id, Kind: "gate", Text: "✓ " + gr.Command})
		}
	}
	if len(guard.globs) > 0 && att.Result.Status == agent.StatusOK {
		changed, err := changedProtected(ctx, task.Dir, guard)
		att.Protected = changed
		if len(changed) > 0 {
			notify(Event{Task: id, Kind: "gate", Text: "! protected files changed: " + strings.Join(changed, ", ")})
		}
		if len(changed) > 0 && !(guard.fail && pass) {
			fw.Emit(protectedInfo(id, plan.number, as, changed))
		}
		if guard.fail && pass && (err != nil || len(changed) > 0) {
			gr := protectGate(guard, changed, err)
			att.Gates = append(att.Gates, gr)
			fw.Emit(gateEvent(id, plan.number, len(att.Gates), as, gr))
			pass = false
			notify(Event{Task: id, Kind: "gate", Text: "✗ " + gr.Command})
		} else if err != nil {
			att.Error = strings.TrimSpace(att.Error + "\nprotect: " + err.Error())
		}
		report(att)
	}
	if pass {
		for _, g := range gates {
			gateN := len(att.Gates) + 1
			fw.Emit(flow.Event{Step: GateStep(id, plan.number, gateN), Parent: as, Kind: flow.KindGate, Task: id, Attempt: plan.number, Status: flow.StatusStarted, Name: g})
			stop := beating(gateBeat, func() { report(att) })
			gr := runGate(ctx, task.Dir, g, plan.gateTimeout)
			stop()
			att.Gates = append(att.Gates, gr)
			fw.Emit(gateEvent(id, plan.number, gateN, as, gr))
			report(att)
			if gr.ExitCode != 0 {
				pass = false
				notify(Event{Task: id, Kind: "gate", Text: fmt.Sprintf("✗ %s (%s)", g, gr.Outcome())})
				break
			}
			notify(Event{Task: id, Kind: "gate", Text: "✓ " + g})
		}
	}

	// Commit failed work so worktree removal cannot lose it.
	sha, note, commitErr := commitChanges(task.Dir, plan.branch, plan.commit)
	if note != "" {
		// No retry may remove or reuse the worktree holding the work.
		att.Error = strings.TrimSpace(att.Error + "\n" + note)
		fw.Emit(flow.Event{Step: as, Parent: TaskStep(id), Kind: flow.KindAttempt, Task: id, Attempt: plan.number, Status: flow.StatusFailed, Reason: attemptEndReason(att), Text: firstLineOf(note)})
		return attemptOutcome{att: att}, fmt.Errorf("%s (%s)", note, task.Dir)
	}
	if sha == "" && commitErr == nil {
		sha, note, commitErr = agentCommit(task.Dir, plan.branch, plan.base)
		if note != "" {
			att.Error = strings.TrimSpace(att.Error + "\n" + note)
		}
	}
	if commitErr != nil {
		att.Error = strings.TrimSpace(att.Error + "\ncommit: " + commitErr.Error())
		fw.Emit(flow.Event{Step: as, Parent: TaskStep(id), Kind: flow.KindAttempt, Task: id, Attempt: plan.number, Status: flow.StatusFailed, Reason: attemptEndReason(att), Text: firstLineOf(commitErr.Error())})
		return attemptOutcome{att: att}, fmt.Errorf("commit failed, changes are uncommitted in %s: %v", task.Dir, commitErr)
	}
	if sha != "" || note != "" {
		fw.Emit(flow.Event{Step: as + "/commit", Parent: as, Kind: flow.KindCommit, Task: id, Attempt: plan.number, Status: flow.StatusOK, Text: firstOf(sha, note)})
	}
	fw.Emit(flow.Event{Step: as, Parent: TaskStep(id), Kind: flow.KindAttempt, Task: id, Attempt: plan.number, Status: attemptStatus(att), Reason: attemptEndReason(att), Text: firstLineOf(att.Result.Error)})
	return attemptOutcome{att: att, commit: sha, pass: pass}, nil
}

// startFailure needs evidence the model was never reached: some adapters report no turns even when it was.
func startFailure(a Attempt) string {
	r := a.Result
	reached := r.Turns != 0 || r.ToolCalls != 0 || r.Final != "" || r.InputTokens != 0 || r.OutputTokens != 0 || r.ContextTokens != 0
	if reached || r.Status != agent.StatusError || r.Error == "" {
		return ""
	}
	return strings.TrimSpace(firstLineOf(r.Error))
}

func gateEvent(id string, attempt, gate int, parent string, gr GateResult) flow.Event {
	status := flow.StatusOK
	reason := ""
	if gr.ExitCode != 0 {
		status, reason = flow.StatusFailed, gr.Outcome()
		if gr.Killed == KilledCanceled {
			status = flow.StatusCanceled
		}
	}
	return flow.Event{Step: gateStep(id, attempt, gate, gr), Parent: parent, Kind: flow.KindGate, Task: id, Attempt: attempt,
		Status: status, Name: gr.Command, Reason: reason, Text: lastLine(gr.Output)}
}

// stop waits for beat, preventing later writes.
func beating(every time.Duration, beat func()) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				beat()
			}
		}
	})
	return func() { close(done); wg.Wait() }
}

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

var runGateShell = tools.RunShell

func runGate(ctx context.Context, dir, command string, timeout time.Duration) GateResult {
	started := time.Now()
	// Gates are operator commands: preserve their environment except git repository overrides.
	res, err := runGateShell(ctx, dir, command, timeout, nil)
	output, decodedUTF16 := decodeGateOutput(res.Output)
	res.Output = output
	gr := GateResult{Command: command, ExitCode: res.ExitCode, Output: tail(res.Output, 4000), Duration: time.Since(started)}
	switch {
	case err != nil:
		gr.ExitCode = -1
		gr.Output = strings.TrimSpace(gr.Output + "\n" + err.Error())
		if ctx.Err() != nil {
			// Cancellation before shell startup is not an infrastructure failure.
			gr.Killed = KilledCanceled
		} else if errors.Is(err, tools.ErrWait) {
			gr.InfrastructureError = err.Error()
		} else {
			gr.InfrastructureError = "shell could not start: " + err.Error()
		}
	case gr.ExitCode == -1 && ctx.Err() != nil:
		// Batch cancellation wins over timeout, but not an actual exit.
		gr.Killed = KilledCanceled
		gr.Output = strings.TrimSpace(gr.Output + "\n[canceled]")
	case res.TimedOut:
		gr.Killed, gr.Timeout = KilledTimeout, agent.Duration(timeout)
		gr.Output = strings.TrimSpace(gr.Output + fmt.Sprintf("\n[timed out after %s]", timeout))
	case gr.ExitCode != 0 && decodedUTF16:
		// Only initial launcher diagnostics count; tests may mention them later.
		prefix := res.Output[:min(len(res.Output), 1024)]
		if code := gateHCSError.FindString(prefix); code != "" {
			gr.InfrastructureError = "WSL launcher: " + code
		} else if strings.Contains(prefix, "The Windows Subsystem for Linux") {
			gr.InfrastructureError = "The Windows Subsystem for Linux launcher could not start"
		}
	}
	return gr
}

var gateHCSError = regexp.MustCompile(`\bHCS_E_[A-Z][A-Z0-9_]*\b`)

// WSL can emit UTF-16 to pipes; decode before truncation or launcher detection.
func decodeGateOutput(s string) (string, bool) {
	m := gateTruncation.FindStringSubmatchIndex(s)
	if m == nil {
		out, decoded, _ := decodeGateText(s)
		return out, decoded
	}
	head, headDecoded, order := decodeGateText(s[:m[0]])
	if order != nil {
		// The cut can end the head inside a character.
		head = strings.TrimSuffix(head, "\ufffd")
	}
	head = trimCutRune(head)
	rest := s[m[1]:]
	tail, tailDecoded, _ := decodeGateText(skipCutRune(rest))
	if order != nil {
		// The tail starts at byte len(head)+dropped of the stream, which fixes its code-unit boundary.
		dropped, _ := strconv.Atoi(s[m[2]:m[3]])
		t16, _, _ := decodeGateText(gateBOM(order) + rest[min(len(rest), (m[0]+dropped)%2):])
		// The cut can start the tail inside a surrogate pair.
		t16 = strings.TrimPrefix(t16, "\ufffd")
		if bad8, bad16 := gateBadRunes(tail), gateBadRunes(t16); bad8 > bad16 || bad8 == bad16 && (tailDecoded || !gatePlainUTF8(tail) || gateOneScript(t16)) {
			tail, tailDecoded = t16, true
		}
	}
	return head + s[m[0]:m[1]] + tail, headDecoded || tailDecoded
}

var gateTruncation = regexp.MustCompile(`\n\n\[\.\.\. (\d+) bytes truncated \.\.\.\]\n\n`)

// decodeGateText decodes s as UTF-16 when it starts as UTF-16, unless it is a UTF-16 prefix ending on a
// newline followed by UTF-8, which stdout and stderr in different encodings give; it returns the
// byte order the text ends in, or nil for UTF-8.
func decodeGateText(s string) (string, bool, binary.ByteOrder) {
	if k, order, bom := gateSplit(s); k > 0 {
		return decodeGateUTF16(s[bom:k], order) + s[k:], true, nil
	}
	order, bom := gateByteOrder(s)
	if order == nil {
		return s, false, nil
	}
	return decodeGateUTF16(s[bom:], order), true, order
}

// gateSplit returns where s turns from UTF-16 ending on a newline to plain UTF-8, or 0.
// A UTF-16 newline holds a NUL byte, so only the one holding the last NUL can be followed by NUL-free text.
func gateSplit(s string) (int, binary.ByteOrder, int) {
	nul := strings.LastIndexByte(s, 0)
	k := nul + 2 - nul%2
	if nul < 0 || k >= len(s) || !gatePlainUTF8(s[k:]) {
		return 0, nil, 0
	}
	order, bom := gateByteOrder(s[:k])
	if order == nil || order.Uint16([]byte(s[k-2:k])) != '\n' || gateBadRunes(decodeGateUTF16(s[bom:k], order)) > 0 {
		return 0, nil, 0
	}
	// A UTF-16 last line in one script can also be valid UTF-8, so the rest must not read as such a line.
	if gateOneScript(decodeGateUTF16(s[k:], order)) {
		return 0, nil, 0
	}
	return k, order, bom
}

// gateByteOrder returns the UTF-16 byte order s starts in, or nil, and the length of its BOM.
func gateByteOrder(s string) (binary.ByteOrder, int) {
	switch {
	case strings.HasPrefix(s, "\xff\xfe"):
		return binary.LittleEndian, 2
	case strings.HasPrefix(s, "\xfe\xff"):
		return binary.BigEndian, 2
	}
	// Without a BOM, require alternating NUL and non-NUL bytes in
	// at least two code units, examining up to the first 32 bytes.
	n := min(len(s)/2, 16)
	if n < 2 {
		return nil, 0
	}
	le, be := true, true
	for i := 0; i < n*2; i += 2 {
		le = le && s[i] != 0 && s[i+1] == 0
		be = be && s[i] == 0 && s[i+1] != 0
	}
	switch {
	case le:
		return binary.LittleEndian, 0
	case be:
		return binary.BigEndian, 0
	}
	return nil, 0
}

func gateBOM(order binary.ByteOrder) string {
	if order == binary.BigEndian {
		return "\xfe\xff"
	}
	return "\xff\xfe"
}

// decodeGateUTF16 decodes s in the given byte order; an odd last byte becomes U+FFFD.
func decodeGateUTF16(s string, order binary.ByteOrder) string {
	units := make([]uint16, 0, (len(s)+1)/2)
	for i := 0; i+1 < len(s); i += 2 {
		units = append(units, order.Uint16([]byte(s[i:i+2])))
	}
	if len(s)%2 != 0 {
		units = append(units, '\ufffd')
	}
	return string(utf16.Decode(units))
}

// gatePlainUTF8 reports whether s is valid UTF-8 with no control bytes but tab, newline, carriage return and escape.
func gatePlainUTF8(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 && c != '\t' && c != '\n' && c != '\r' && c != 0x1b || c == 0x7f {
			return false
		}
	}
	return utf8.ValidString(s)
}

// gateOneScript reports whether s is printable text in at most one script besides Common and Inherited.
// Real UTF-16 text reads so; UTF-8 bytes read as UTF-16 scatter over scripts or hit unassigned code points.
func gateOneScript(s string) bool {
	script := ""
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r' || r == 0x1b:
			continue
		case r == utf8.RuneError || !unicode.IsGraphic(r):
			return false
		case unicode.In(r, unicode.Common, unicode.Inherited):
			continue
		}
		if k := gateScript(r); script == "" {
			script = k
		} else if k != script {
			return false
		}
	}
	return true
}

// gateCJK are the scripts written together in Chinese, Japanese and Korean text.
var gateCJK = []*unicode.RangeTable{unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul, unicode.Bopomofo}

func gateScript(r rune) string {
	for _, t := range gateCJK {
		if unicode.Is(t, r) {
			return "CJK"
		}
	}
	for name, t := range unicode.Scripts {
		if unicode.Is(t, r) {
			return name
		}
	}
	return ""
}

// gateBadRunes counts the NULs and invalid or replaced characters in s.
func gateBadRunes(s string) int {
	n := 0
	for _, r := range s {
		if r == 0 || r == utf8.RuneError {
			n++
		}
	}
	return n
}

// trimCutRune drops a UTF-8 character that s ends inside.
func trimCutRune(s string) string {
	for i := len(s) - 1; i >= max(0, len(s)-3); i-- {
		if utf8.RuneStart(s[i]) {
			if !utf8.FullRuneInString(s[i:]) {
				return s[:i]
			}
			break
		}
	}
	return s
}

// skipCutRune drops the continuation bytes a cut leaves before the first whole UTF-8 character.
func skipCutRune(s string) string {
	for k := 0; k < 3 && s != "" && s[0]&0xc0 == 0x80; k++ {
		s = s[1:]
	}
	return s
}

// Commit despite cancellation to leave a consistent branch; note says why off-branch work stayed uncommitted.
func commitChanges(wt, branch, message string) (sha, note string, err error) {
	if _, err := os.Stat(wt); err != nil {
		return "", "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	g, err := newAgentGit(ctx, wt)
	if err != nil {
		return "", "", err
	}
	defer g.close()
	status, err := g.stdout(ctx, "status", "--porcelain", "-z", "--no-renames", "--ignore-submodules=all")
	if err != nil || status == "" {
		return "", "", err
	}
	if branch != "" {
		// A merge takes the task's branch, so a commit anywhere else would be recorded for nothing.
		// Prints nothing for a detached HEAD and names an unborn branch, failing only when git does.
		head, err := g.stdout(ctx, "branch", "--show-current")
		if err != nil {
			return "", "", err
		}
		if head = strings.TrimSuffix(strings.TrimSuffix(head, "\n"), "\r"); head != branch {
			where := "HEAD is detached"
			if head != "" {
				where = "HEAD is on branch " + head
			}
			return "", "work left uncommitted in the worktree: " + where + ", not the task's branch " + branch, nil
		}
	}
	// Stage reported paths: add -A ignores diff.ignoreSubmodules.
	var paths []string
	for _, entry := range strings.Split(status, "\x00") {
		if len(entry) > 3 {
			paths = append(paths, entry[3:])
		}
	}
	if _, err := g.in(ctx, strings.Join(paths, "\x00"),
		"--literal-pathspecs", "add", "-A", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
		return "", "", err
	}
	if _, err := g.out(ctx, "commit", "-q", "-m", message); err != nil {
		return "", "", err
	}
	sha, err = g.out(ctx, "rev-parse", "HEAD")
	return sha, "", err
}

// gitTips is the task branch's tip and HEAD's commit, so a commit off the branch shows too; "" without a worktree.
func gitTips(wt, branch string) (string, error) {
	if _, err := os.Stat(wt); err != nil || branch == "" {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	g, err := newAgentGit(ctx, wt)
	if err != nil {
		return "", err
	}
	defer g.close()
	out, err := g.stdout(ctx, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	tip := ""
	for _, line := range strings.Split(out, "\n") {
		if name, head, ok := strings.Cut(line, " "); ok && name == "refs/heads/"+branch {
			tip = head
		}
	}
	// An orphan branch has no HEAD commit yet; that is a move, not a failed read.
	head, _ := g.stdout(ctx, "rev-parse", "-q", "--verify", "HEAD")
	return tip + " " + strings.TrimSpace(head), nil
}

// agentCommit returns the branch's tip when the agent committed its own work on it
// and left nothing for commitChanges, or "" while the branch is still at the base;
// note says why a moved tip was left out. A commit on no branch is never taken.
func agentCommit(wt, branch, base string) (sha, note string, err error) {
	if _, err := os.Stat(wt); err != nil || branch == "" || base == "" {
		return "", "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	g, err := newAgentGit(ctx, wt)
	if err != nil {
		return "", "", err
	}
	defer g.close()
	// for-each-ref matches descendants too; require the exact ref.
	out, err := g.stdout(ctx, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/"+branch)
	if err != nil {
		return "", "", err
	}
	for _, line := range strings.Split(out, "\n") {
		name, head, ok := strings.Cut(line, " ")
		if !ok || name != "refs/heads/"+branch || head == base {
			continue
		}
		// A tip that dropped the base would revert it when merged.
		missing, err := g.stdout(ctx, "rev-list", "--count", head+".."+base)
		if err != nil {
			return "", "", err
		}
		if strings.TrimSpace(missing) != "0" {
			return "", "commit not recorded: the branch tip " + tools.ShortSHA(head) + " does not contain the base", nil
		}
		return head, "", nil
	}
	return "", "", nil
}

// Agent config must not execute hooks, drivers, signing or maintenance.
// Use GIT_CONFIG_KEY/VALUE (-c splits filter names at '='); never enter submodules, whose config the agent writes.
type agentGit struct {
	dir   string
	hooks string
	temp  bool // hooks is an empty directory close removes
	pin   []string
}

// Disable all hooks in agent worktrees.
func newAgentGit(ctx context.Context, dir string) (*agentGit, error) {
	g, err := pinnedGit(ctx, dir)
	if err != nil {
		return nil, err
	}
	if g.hooks, err = os.MkdirTemp("", "hrn-empty-hooks-"); err != nil {
		return nil, err
	}
	g.temp = true
	return g, nil
}

// Trust operator hooks, never hooks paths in shared config.
func newRepoGit(ctx context.Context, root string) (*agentGit, error) {
	g, err := pinnedGit(ctx, root)
	if err != nil {
		return nil, err
	}
	if g.hooks = operatorHooksPath(ctx, g.dir, g.pin); g.hooks != "" {
		return g, nil
	}
	common, err := gitStdoutEnv(ctx, g.dir, g.pin, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	g.hooks = filepath.Join(strings.TrimSpace(common), "hooks")
	return g, nil
}

func operatorHooksPath(ctx context.Context, dir string, pin []string) string {
	out, err := gitStdoutEnv(ctx, dir, pin, "config", "--show-scope", "--get-regexp", `^core\.hookspath$`)
	if err != nil {
		return ""
	}
	path := ""
	for _, line := range strings.Split(out, "\n") {
		scope, entry, _ := strings.Cut(line, "\t")
		switch scope {
		case "global", "system", "command":
			_, path, _ = strings.Cut(entry, " ")
		}
	}
	return path
}

// Find .git directly; shared core.worktree can redirect rev-parse.
func repoRoot(ctx context.Context, dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for d := abs; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			g, err := pinnedGit(ctx, d)
			if err != nil {
				return "", err
			}
			top, err := gitOutEnv(ctx, d, g.pin, "", "rev-parse", "--show-toplevel")
			if err != nil {
				return "", err
			}
			return filepath.Clean(filepath.FromSlash(top)), nil
		}
		if filepath.Dir(d) == d {
			return "", fmt.Errorf("%s is not inside a git work tree", dir)
		}
	}
}

func pinnedGit(ctx context.Context, dir string) (*agentGit, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	// Set GIT_WORK_TREE before discovery to bypass hostile core.worktree.
	work := "GIT_WORK_TREE=" + dir
	gitDir, err := gitStdoutEnv(ctx, dir, []string{work}, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	// Disable lazy fetch to prevent shared promisor upload-pack execution.
	return &agentGit{dir: dir, pin: []string{"GIT_DIR=" + strings.TrimSpace(gitDir), work, "GIT_NO_LAZY_FETCH=1"}}, nil
}

func (g *agentGit) close() {
	if g.temp {
		_ = os.RemoveAll(g.hooks)
	}
}

func (g *agentGit) out(ctx context.Context, args ...string) (string, error) {
	return gitOutEnv(ctx, g.dir, g.env(ctx), "", args...)
}

func (g *agentGit) in(ctx context.Context, stdin string, args ...string) (string, error) {
	return gitOutEnv(ctx, g.dir, g.env(ctx), stdin, args...)
}

func (g *agentGit) stdout(ctx context.Context, args ...string) (string, error) {
	return gitStdoutEnv(ctx, g.dir, g.env(ctx), args...)
}

// Rescan: agent descendants can install filters between commands.
func (g *agentGit) env(ctx context.Context) []string {
	return append(slices.Clone(g.pin), configEnv(agentConfigOverrides(ctx, g.dir, g.pin, g.hooks))...)
}

// Append guards last so they override operator entries.
func configEnv(pairs [][2]string) []string {
	n, err := strconv.Atoi(os.Getenv("GIT_CONFIG_COUNT"))
	if err != nil || n < 0 {
		n = 0
	}
	env := make([]string, 0, 2*len(pairs)+1)
	for i, p := range pairs {
		env = append(env,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", n+i, p[0]),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", n+i, p[1]))
	}
	return append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", n+len(pairs)))
}

// Block local/worktree programs; trust global/system drivers.
func agentConfigOverrides(ctx context.Context, dir string, pin []string, hooks string) [][2]string {
	pairs := [][2]string{
		{"core.hooksPath", hooks},
		{"maintenance.auto", "false"},
		{"gc.auto", "0"},
		{"diff.ignoreSubmodules", "all"},
		{"status.submoduleSummary", "false"},
		{"submodule.recurse", "false"},
	}
	out, err := gitStdoutEnv(ctx, dir, pin, "config", "--show-scope", "--get-regexp",
		`^(filter\..*\.(clean|smudge|process)|merge\..*\.driver|core\.fsmonitor|gpg\..*program|gpg\.ssh\.defaultkeycommand|extensions\.partialclone|remote\..*\.(promisor|partialclonefilter))$`)
	if err != nil {
		return pairs
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		scope, entry, _ := strings.Cut(line, "\t")
		switch scope {
		case "global", "system", "command":
			continue
		}
		key, _, _ := strings.Cut(entry, " ")
		switch {
		case key == "extensions.partialclone" || strings.HasPrefix(key, "remote."):
			// Promisor remotes could start a transport during hrn's git commands.
			if !seen["promisor"] {
				seen["promisor"] = true
				pairs = append(pairs, [2]string{"protocol.allow", "never"})
			}
			continue
		case key == "core.fsmonitor":
			if !seen[key] {
				seen[key] = true
				pairs = append(pairs, [2]string{"core.fsmonitor", "false"})
			}
			continue
		case strings.HasPrefix(key, "gpg."):
			if !seen["gpg."] {
				seen["gpg."] = true
				pairs = append(pairs, [2]string{"commit.gpgSign", "false"}, [2]string{"merge.verifySignatures", "false"})
			}
			continue
		}
		if rest, ok := strings.CutPrefix(key, "merge."); ok {
			// Fail closed on merge drivers; conflict cleanup will undo the merge.
			if name, ok := strings.CutSuffix(rest, ".driver"); ok && name != "" && !seen["merge:"+name] {
				seen["merge:"+name] = true
				pairs = append(pairs, [2]string{"merge." + name + ".driver", "false"})
			}
			continue
		}
		rest, ok := strings.CutPrefix(key, "filter.")
		if !ok {
			continue
		}
		dot := strings.LastIndexByte(rest, '.')
		if dot <= 0 {
			continue
		}
		switch rest[dot+1:] {
		case "clean", "smudge", "process":
		default:
			continue
		}
		// Prefix filter keys to avoid collisions with guard markers.
		name := rest[:dot]
		if !seen["filter:"+name] {
			seen["filter:"+name] = true
			pairs = append(pairs,
				[2]string{"filter." + name + ".clean", ""},
				[2]string{"filter." + name + ".smudge", ""},
				[2]string{"filter." + name + ".process", ""},
				[2]string{"filter." + name + ".required", "false"},
			)
		}
	}
	return pairs
}

func hasChanges(wt string) (bool, error) {
	if _, err := os.Stat(wt); err != nil {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// Preserve untracked work on removal; startup must allow .hrn/worktrees.
	lines, err := dirtyPaths(ctx, wt, true)
	return len(lines) > 0, err
}

func dirtyPaths(ctx context.Context, dir string, untracked bool) ([]string, error) {
	g, err := newAgentGit(ctx, dir)
	if err != nil {
		return nil, err
	}
	defer g.close()
	args := []string{"status", "--porcelain", "--ignore-submodules=all"}
	if !untracked {
		args = append(args, "--untracked-files=no")
	}
	status, err := g.out(ctx, args...)
	if err != nil || status == "" {
		return nil, err
	}
	return strings.Split(status, "\n"), nil
}

// Remove deletes owned branches and worktrees, preserving uncommitted work and slots.
func Remove(res Result) error {
	var errs []error
	for _, tr := range res.Tasks {
		if _, err := removeTask(res, tr); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", tr.ID, err))
		}
	}
	return errors.Join(errs...)
}

// RemoveTasks returns removed task IDs even when other named tasks fail.
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

var branchProbe = func(ctx context.Context, dir, ref string) (string, error) {
	return gitOut(ctx, dir, "for-each-ref", "--format=%(refname)", ref)
}

func removeTask(res Result, tr TaskResult) (bool, error) {
	// Bound probes; claim removal only after both probes succeed.
	branchState := func() (exists, ok bool) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		ref := "refs/heads/" + tr.Branch
		out, err := branchProbe(ctx, res.Dir, ref)
		if err != nil {
			return false, false
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.TrimSpace(line) == ref {
				return true, true
			}
		}
		return false, true
	}
	hadBranch := func() bool { exists, ok := branchState(); return ok && exists }
	branchGone := func() bool { exists, ok := branchState(); return ok && !exists }
	if branchOnly(tr) {
		had := hadBranch()
		err := removeSlotBranch(res.Dir, tr)
		return had && branchGone(), err
	}
	if tr.Worktree == "" || notMine(res, tr) {
		return false, nil
	}
	// Deregistered directories' status describes the surrounding repo.
	registered, err := isWorktree(res.Dir, tr.Worktree)
	if err != nil {
		return false, err
	}
	if registered {
		if dirty, err := hasChanges(tr.Worktree); err == nil && dirty {
			return false, fmt.Errorf("%s has uncommitted changes; commit or discard them first", tr.Worktree)
		}
	}
	had := hadBranch()
	err = removeWorktree(res.Dir, tr)
	stillRegistered, checkErr := isWorktree(res.Dir, tr.Worktree)
	return (registered && checkErr == nil && !stillRegistered) || (had && branchGone()), err
}

// Only BranchMade proves slotless ownership; legacy records cannot.
func branchOnly(tr TaskResult) bool {
	return tr.Slot != 0 || (tr.Worktree == "" && tr.Status == StatusRunning && tr.BranchMade)
}

// Paths precede creation; only BranchMade proves ownership.
// Legacy records instead exclude tasks that failed because the path or branch was taken.
func notMine(res Result, tr TaskResult) bool {
	if res.Ownership >= OwnershipClaim {
		return !tr.BranchMade
	}
	return foundTaken(tr)
}

// Match legacy pre-attempt failures only; hooks can say "already exists".
func foundTaken(tr TaskResult) bool {
	return tr.Slot == 0 && !tr.BranchMade && len(tr.Attempts) == 0 &&
		(tr.Error == takenError("branch", tr.Branch) || tr.Error == takenError("worktree", tr.Worktree))
}

// Keep text stable for legacy ownership checks.
func takenError(what, name string) string {
	return fmt.Sprintf("%s %s already exists; remove it or pick another task id", what, name)
}

// Never delete a branch currently checked out by a task or operator.
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

// Resolve Windows short names and separators before ignoring case.
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

func addWorktree(ctx context.Context, root string, tr TaskResult, base string) error {
	worktreeMu.Lock()
	defer worktreeMu.Unlock()
	g, err := newRepoGit(ctx, root)
	if err != nil {
		return err
	}
	defer g.close()
	// Never retry: metadata failure may follow branch creation.
	cmd := exec.CommandContext(ctx, "git", "-C", root, "worktree", "add", "-q", "-b", tr.Branch, tr.Worktree, base)
	cmd.Env = append(gitenv.WithoutRepoVars(cmd.Environ()), g.env(ctx)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git worktree add: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// Only registered worktrees or surviving branches prove ownership.
// If listing fails, trust only the branch; stray paths may be reused.
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
		// Windows locks leave deregistered directories; remove only if empty.
		_, _ = gitOut(ctx, root, "worktree", "prune")
		leftover = RemoveEmptyDir(tr.Worktree)
	}
	if _, err := gitOut(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+tr.Branch); err == nil {
		if _, err := gitOut(ctx, root, "branch", "-D", tr.Branch); err != nil {
			return errors.Join(leftover, err)
		}
	}
	return leftover
}

// RemoveEmptyDir removes dir only when empty: files in a directory git no longer lists are in no branch.
func RemoveEmptyDir(dir string) error {
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

// SaveResult saves a batch record using the same path as Run.
func SaveResult(res Result) error { return save(res) }

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
	// Rename complete JSON so readers never see partial records.
	// Windows needs os.Root POSIX rename and FILE_SHARE_DELETE; retry other readers.
	root, err := os.OpenRoot(batchesDir())
	if err != nil {
		return err
	}
	defer root.Close()
	name := res.ID + ".json"
	if err := root.WriteFile(name+".tmp", data, 0o644); err != nil {
		return err
	}
	const budget = 2 * time.Second
	for delay, waited := 10*time.Millisecond, time.Duration(0); ; {
		err = saveRename(root, name+".tmp", name)
		if err == nil {
			return nil
		}
		if !renameBusy(err) || waited >= budget {
			_ = root.Remove(name + ".tmp")
			return err
		}
		pause := min(delay, budget-waited)
		saveSleep(pause)
		waited += pause
		delay = min(2*delay, 200*time.Millisecond)
	}
}

// List returns records newest first and counts unreadable files.
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

// Find returns the task with the given ID.
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

// Diff compares a task's commit or uncommitted fresh work with the base.
// Untracked paths are separate because a diff cannot show them.
func Diff(ctx context.Context, res Result, taskID string) (diff string, untracked []string, err error) {
	tr, err := res.Find(taskID)
	if err != nil {
		return "", nil, err
	}
	if tr.Commit != "" {
		// Branch removal permits commit collection; explain "bad object".
		if _, err := gitOut(ctx, res.Dir, "cat-file", "-e", tr.Commit+"^{commit}"); err != nil {
			return "", nil, fmt.Errorf("task %s: commit %s is no longer in %s, so its branch was removed",
				taskID, tools.ShortSHA(tr.Commit), res.Dir)
		}
		out, err := gitStdout(ctx, res.Dir, "diff", "--no-ext-diff", "--no-textconv", "--ignore-submodules=all", "--end-of-options", res.Base+".."+tr.Commit, "--")
		return out, nil, err
	}
	if tr.Slot != 0 {
		// Released slots may now contain another task's work.
		return "", nil, nil
	}
	if _, err := os.Stat(tr.Worktree); err != nil {
		return "", nil, fmt.Errorf("task %s committed nothing and its worktree is gone", taskID)
	}
	g, err := newAgentGit(ctx, tr.Worktree)
	if err != nil {
		return "", nil, err
	}
	defer g.close()
	against := "HEAD"
	if _, err := g.stdout(ctx, "rev-parse", "-q", "--verify", "HEAD^{commit}"); err != nil {
		// An orphan checkout leaves HEAD unborn; its files still compare with the base.
		against = res.Base
	}
	out, err := g.stdout(ctx, "diff", "--no-ext-diff", "--no-textconv", "--ignore-submodules=all", "--end-of-options", against, "--")
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

// CheckoutTree exports a commit with guards against agent-written shared config.
func CheckoutTree(ctx context.Context, repo, commit, dir string) error {
	g, err := newAgentGit(ctx, repo)
	if err != nil {
		return err
	}
	defer g.close()
	scratch, err := os.MkdirTemp("", "hrn-index-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	env := append(g.env(ctx), "GIT_INDEX_FILE="+filepath.Join(scratch, "index"), "GIT_WORK_TREE="+dir)
	// Block fsmonitor and aliases like GIT~1 that could overwrite .git.
	guard := []string{"-c", "core.fsmonitor=false", "-c", "core.protectNTFS=true", "-c", "core.protectHFS=true"}
	if _, err := gitOutEnv(ctx, g.dir, env, "", append(slices.Clone(guard), "read-tree", commit+"^{tree}")...); err != nil {
		return err
	}
	_, err = gitOutEnv(ctx, g.dir, env, "", append(guard, "checkout-index", "-a")...)
	return err
}

// MergeResult records one task's merge outcome.
type MergeResult struct {
	Task     string `json:"task"`
	Branch   string `json:"branch"`
	Commit   string `json:"commit,omitempty"`   // the merge or squash commit this run made
	Skipped  string `json:"skipped,omitempty"`  // why nothing was merged
	Conflict bool   `json:"conflict,omitempty"` // git reported conflicting paths
	Error    string `json:"error,omitempty"`    // any other failure, in git's words
}

// Merge merges passing tasks in spec order, restoring the tree on failure.
// It returns completed results with the error.
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

func alreadyIn(ctx context.Context, root, branch string) bool {
	_, err := gitOut(ctx, root, "merge-base", "--is-ancestor", branch, "HEAD")
	return err == nil
}

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

var errNothingStaged = errors.New("nothing to commit")

// Hook, identity and cancellation failures are not conflicts.
func mergeBranch(ctx context.Context, root string, tr TaskResult, squash bool) (commit string, conflicted bool, err error) {
	args := []string{"merge", "--no-ff", "--no-edit", tr.Branch}
	if squash {
		args = []string{"merge", "--squash", tr.Branch}
	}
	g, err := newRepoGit(ctx, root)
	if err != nil {
		return "", false, err
	}
	defer g.close()
	if _, err := g.out(ctx, args...); err != nil {
		return "", hasUnmerged(g), undoMerge(root, err)
	}
	if squash {
		if staged, err := hasStagedChanges(ctx, g); err != nil {
			return "", false, undoMerge(root, err)
		} else if !staged {
			return "", false, undoMerge(root, errNothingStaged)
		}
		if _, err := g.out(ctx, "commit", "-q", "-m", tr.ID+": "+tools.OneLine(tr.Prompt, 60)); err != nil {
			return "", hasUnmerged(g), undoMerge(root, err)
		}
	}
	commit, err = gitOut(ctx, root, "rev-parse", "HEAD")
	return commit, false, err
}

// Use a fresh context during canceled-merge cleanup.
func hasUnmerged(g *agentGit) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := g.out(ctx, "ls-files", "--unmerged")
	return err == nil && out != ""
}

func hasStagedChanges(ctx context.Context, g *agentGit) (bool, error) {
	// Ask for names: --quiet conflates differences and command errors.
	out, err := g.out(ctx, "diff", "--cached", "--name-only")
	return out != "", err
}

// Cleanup outlives cancellation; squash has no MERGE_HEAD to abort.
// Reset to HEAD is safe only after the clean-tree check.
func undoMerge(root string, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	g, err := newRepoGit(ctx, root)
	if err != nil {
		return fmt.Errorf("%w; and the tree could not be restored: %v", cause, err)
	}
	defer g.close()
	if _, err := g.out(ctx, "merge", "--abort"); err == nil {
		return cause
	}
	if _, err := g.out(ctx, "reset", "--hard", "HEAD"); err != nil {
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

// Head returns the repository's current commit before a batch merge starts.
func Head(ctx context.Context, dir string) (string, error) {
	return gitOut(ctx, dir, "rev-parse", "HEAD")
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	return gitOutEnv(ctx, dir, nil, "", args...)
}

func gitOutEnv(ctx context.Context, dir string, env []string, stdin string, args ...string) (string, error) {
	out, err := gitretry.Output(ctx, func() ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(append(gitenv.WithoutRepoVars(cmd.Environ()), env...), "LC_ALL=C", "LANGUAGE=")
		cmd.Stdin = strings.NewReader(stdin)
		return cmd.CombinedOutput()
	})
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), text)
	}
	return text, nil
}

// Exclude stderr to avoid corrupting patches.
func gitStdout(ctx context.Context, dir string, args ...string) (string, error) {
	return gitStdoutEnv(ctx, dir, nil, args...)
}

func gitStdoutEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(gitenv.WithoutRepoVars(cmd.Environ()), env...)
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

// Validate MCP paths early; empty disables, absent inherits.
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
