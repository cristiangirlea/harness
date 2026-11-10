package batch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/purpose"
	"github.com/cristiangirlea/harness/internal/resolve"
	"github.com/cristiangirlea/harness/internal/runlog"
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
	RetryK      int            `json:"retry_k,omitempty"`
	// Protect uses slash globs relative to the repository.
	Protect     []string `json:"protect,omitempty"`
	ProtectMode string   `json:"protect_mode,omitempty"`
	// Worktrees selects reusable slots or fresh task worktrees.
	Worktrees string `json:"worktrees,omitempty"`
	Tasks     []Task `json:"tasks"`
	// Base is the commit tasks branch from instead of HEAD; set by callers, never by a spec file.
	Base string `json:"-"`
	// NoTurnDefault keeps eval cells comparable across backends.
	NoTurnDefault bool `json:"-"`
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
	RetryK      int            `json:"retry_k,omitempty"`
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
	BranchMade bool             `json:"branch_made,omitempty"`
	Commit     string           `json:"commit,omitempty"` // the agent's changes; empty when it changed nothing
	Attempts   []Attempt        `json:"attempts"`
	Candidates []RetryCandidate `json:"candidates,omitempty"`
	Error      string           `json:"error,omitempty"`
	Duration   time.Duration    `json:"duration_ns"`
}

type RetryCandidate struct {
	Branch        string       `json:"branch,omitempty"`
	Worktree      string       `json:"worktree,omitempty"`
	Slot          int          `json:"slot,omitempty"`
	BranchMade    bool         `json:"branch_made,omitempty"`
	BriefAddition string       `json:"brief_addition,omitempty"`
	RunID         string       `json:"run_id,omitempty"`
	Gates         []GateResult `json:"gates,omitempty"`
	Tokens        int          `json:"tokens,omitempty"`
	Winner        bool         `json:"winner,omitempty"`
}

func pickRetryWinner(candidates []RetryCandidate) int {
	winner := -1
	for i, c := range candidates {
		passes := true
		for _, gate := range c.Gates {
			if gate.ExitCode != 0 || gate.Killed != "" || gate.InfrastructureError != "" {
				passes = false
				break
			}
		}
		if passes && (winner < 0 || c.Tokens < candidates[winner].Tokens) {
			winner = i
		}
	}
	return winner
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
	Gated bool `json:"gated,omitempty"`
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

// Event is progress for a caller; Kind is start, tool, run, gate, retry, switch, wait, issue or done.
type Event struct {
	Task string
	Kind string
	Text string
}

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
	if err := agent.ValidateOptions(s.Options); err != nil {
		return err
	}
	if err := checkRetryK(s.RetryK); err != nil {
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
		if err := agent.ValidateOptions(t.Options.Over(s.Options)); err != nil {
			return fmt.Errorf("task %s: %w", t.ID, err)
		}
		if err := agent.CheckCompactWindow(firstOf(t.Backend, s.Backend), t.Options.Over(s.Options).CompactWindow); err != nil {
			return fmt.Errorf("task %s: %w", t.ID, err)
		}
		if err := checkRetryK(t.RetryK); err != nil {
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
	anyRetryK := s.RetryK >= 2
	for _, t := range s.Tasks {
		anyRetryK = anyRetryK || t.RetryK >= 2
	}
	if anyRetryK {
		for _, t := range s.Tasks {
			for _, other := range s.Tasks {
				suffix, ok := strings.CutPrefix(t.ID, other.ID+"-k")
				if ok && suffix != "" && strings.Trim(suffix, "0123456789") == "" {
					return fmt.Errorf("task id %q collides with retry candidates of %q", t.ID, other.ID)
				}
			}
		}
	}
	return nil
}

func checkRetryK(k int) error {
	if k != 0 && (k < 2 || k > 5) {
		return fmt.Errorf("retry_k %d: use 2 through 5, or omit to disable", k)
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
	backend = firstOf(backend, resolve.DefaultBackendNameIn(runlog.Home()))
	for _, name := range retry {
		if name != backend {
			return fmt.Errorf("retry names %s, but retrieval pins the task to its backend %s: retry on %s only, or turn retrieval off", name, backend, backend)
		}
	}
	return nil
}

func retrievalOn(opts agent.Options) bool { return opts.Retrieval != "" && opts.Retrieval != "off" }

func checkBackends(backend string, retry *[]string) error {
	p, err := resolve.BackendPreferences(runlog.Home())
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
