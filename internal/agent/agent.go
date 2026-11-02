// Package agent defines the shared backend contract.
package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/cristiangirlea/harness/internal/kernel"
	"github.com/cristiangirlea/harness/internal/tools"
)

type Mode = kernel.Mode

type ApprovalRequest = kernel.ApprovalRequest

const (
	ModeReadOnly = kernel.ModeReadOnly
	ModeAsk      = kernel.ModeAsk
	ModeAuto     = kernel.ModeAuto
	ModeYolo     = kernel.ModeYolo
)

func ParseMode(s string) (Mode, error) { return kernel.ParseMode(s) }

type Task = kernel.Task
type Conversation = kernel.Conversation
type Message = kernel.Message

type EventKind = kernel.EventKind
type Quota = kernel.Quota
type QuotaWindow = kernel.QuotaWindow
type Usage = kernel.Usage
type Event = kernel.Event
type Status = kernel.Status

const (
	EventText       = kernel.EventText
	EventThinking   = kernel.EventThinking
	EventToolCall   = kernel.EventToolCall
	EventToolResult = kernel.EventToolResult
	EventInfo       = kernel.EventInfo
	EventError      = kernel.EventError
	EventSession    = kernel.EventSession
	EventUsage      = kernel.EventUsage
	EventCompaction = kernel.EventCompaction
	EventQuota      = kernel.EventQuota
	StatusOK        = kernel.StatusOK
	StatusError     = kernel.StatusError
	StatusRefused   = kernel.StatusRefused
	StatusMaxTurns  = kernel.StatusMaxTurns
	StatusTimeout   = kernel.StatusTimeout
	StatusBudget    = kernel.StatusBudget
	StatusCanceled  = kernel.StatusCanceled
	StatusLimited   = kernel.StatusLimited
)

type Result = kernel.Result

type Backend = kernel.Backend

// Capabilities describes options enforceable by a supervisor.
type Capabilities struct {
	Modes     []Mode   `json:"modes"`
	Efforts   []string `json:"efforts"`
	Sessions  string   `json:"sessions"`
	Options   []string `json:"options"`
	Approvals bool     `json:"approvals"`
}

func BackendCapabilities(b Backend) Capabilities {
	c := Capabilities{Modes: []Mode{}, Efforts: []string{}, Sessions: "none", Options: []string{}}
	if provider, ok := b.(interface{ Capabilities() Capabilities }); ok {
		c = provider.Capabilities()
	}
	if c.Modes == nil {
		c.Modes = []Mode{}
	}
	if c.Efforts == nil {
		c.Efforts = []string{}
	}
	if c.Options == nil {
		c.Options = []string{}
	}
	if c.Sessions == "" {
		c.Sessions = "none"
	}
	return c
}

func (b *APIBackend) Capabilities() Capabilities {
	return Capabilities{
		Modes:   []Mode{ModeReadOnly, ModeAuto, ModeYolo},
		Efforts: append([]string{}, EffortLevels...), Sessions: "fork",
		Options: []string{"model", "mode", "effort", "timeout", "max_turns", "mcp"},
	}
}

var registry = map[string]Backend{}

func Register(b Backend) { registry[b.Name()] = b }

func Get(name string) (Backend, error) {
	b, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown backend %q (have %v)", name, Names())
	}
	return b, nil
}

func Names() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Run is what callers use instead of Backend.Run: it enforces the guard and counts tool calls.
func Run(ctx context.Context, b Backend, task Task, emit func(Event)) Result {
	// Entry points validate first; this catches one that does not.
	if task.AllowDangerous && tools.Guarded() {
		emit(Event{Time: time.Now(), Kind: EventError, Text: ErrGuardedRun.Error()})
		return Result{Backend: b.Name(), Status: StatusRefused, Error: ErrGuardedRun.Error()}
	}
	if task.InheritEnv && tools.Guarded() {
		emit(Event{Time: time.Now(), Kind: EventError, Text: ErrGuardedInherit.Error()})
		return Result{Backend: b.Name(), Status: StatusRefused, Error: ErrGuardedInherit.Error()}
	}
	prepared, state, err := prepareRetrieval(ctx, b.Name(), task)
	if err != nil {
		return Result{Backend: b.Name(), Status: StatusError, Error: err.Error(), Retrieval: state}
	}
	task = prepared
	// After retrieval, so the block is not part of its query.
	task.Prompt = commitBlock(task) + task.Prompt
	if state != nil {
		emit(Event{Time: time.Now(), Kind: EventInfo, Text: fmt.Sprintf("retrieval: mode=%s replayed=%t failure=%s", state.Mode, state.Replayed, state.Failure)})
	}
	calls := 0
	res := b.Run(ctx, task, func(e Event) {
		if e.Kind == EventToolCall && !e.Partial {
			calls++
		}
		emit(e)
	})
	res.Retrieval = state
	if task.Durable == nil {
		res.ToolCalls = calls
	}
	return res
}

func finish(res *Result, started time.Time) Result {
	res.Duration = time.Since(started)
	return *res
}

// Version is reported to MCP peers as the hrn version.
const Version = "0.2.0"

// DefaultBackendName prefers API credentials, otherwise claude.
func DefaultBackendName() string {
	if b, ok := registry["api"]; ok {
		if _, err := b.Detect(); err == nil {
			return "api"
		}
	}
	return "claude"
}

// MinTaskBudget is the smallest task budget the API accepts.
const MinTaskBudget = kernel.MinTaskBudget

// ValidateLimits rejects unsupported execution limits.
func ValidateLimits(timeout time.Duration, maxCostUSD float64, taskBudget int) error {
	return kernel.ValidateLimits(timeout, maxCostUSD, taskBudget)
}

// Enforce deadlines in each backend so every caller gets the same limit.
func withTimeout(ctx context.Context, task Task) (context.Context, context.CancelFunc) {
	if task.Timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, time.Duration(task.Timeout))
}

func stopStatus(ctx context.Context) Status {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return StatusTimeout
	}
	return StatusCanceled
}

// AppliesLean reports whether b honours Options.Lean.
func AppliesLean(b Backend) bool {
	l, ok := b.(interface{ AppliesLean() bool })
	return ok && l.AppliesLean()
}

// AppliesSystemPrompt reports whether b honours Options.AppendSystemPrompt.
func AppliesSystemPrompt(b Backend) bool {
	s, ok := b.(interface{ AppliesSystemPrompt() bool })
	return ok && s.AppliesSystemPrompt()
}

// AppliesNoSkills reports whether b honours Options.NoSkills.
func AppliesNoSkills(b Backend) bool {
	s, ok := b.(interface{ AppliesNoSkills() bool })
	return ok && s.AppliesNoSkills()
}

func unenforced(task Task, c *CLIBackend) []string {
	var out []string
	if task.MaxCostUSD > 0 {
		out = append(out, "max_cost")
	}
	if task.TaskBudget > 0 {
		out = append(out, "task_budget")
	}
	if task.Effort != "" && !c.mapsEffort {
		out = append(out, "effort")
	}
	if task.Lean != nil && *task.Lean && !c.mapsLean { // the default is not a request
		out = append(out, "lean")
	}
	if task.CompactWindow != "" && !c.mapsCompact {
		out = append(out, "compact_window")
	}
	if task.AppendSystemPrompt != "" && !c.mapsSystemPrompt {
		out = append(out, "append_system_prompt")
	}
	if task.NoSkills && !c.mapsNoSkills {
		out = append(out, "no_skills")
	}
	if task.Skills != nil && *task.Skills && !c.mapsNoSkills {
		out = append(out, "skills")
	}
	return out
}
