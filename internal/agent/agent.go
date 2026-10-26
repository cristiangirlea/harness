// Package agent defines the shared backend contract.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/cristiangirlea/harness/internal/retrieval"
	"github.com/cristiangirlea/harness/internal/tools"
)

// Mode controls what an agent may do without asking.
type Mode string

const (
	ModeReadOnly Mode = "readonly" // read and search only
	ModeAsk      Mode = "ask"      // reads auto-approved; writes and commands need approval
	ModeAuto     Mode = "auto"     // reads and file edits auto-approved; commands need approval
	ModeYolo     Mode = "yolo"     // everything auto-approved
)

func ParseMode(s string) (Mode, error) {
	switch m := Mode(s); m {
	case ModeReadOnly, ModeAsk, ModeAuto, ModeYolo:
		return m, nil
	}
	return "", fmt.Errorf("unknown mode %q (want readonly, ask, auto, yolo)", s)
}

// ApprovalRequest asks for a human tool-call decision.
type ApprovalRequest struct {
	Tool    string
	Summary string
}

type Task struct {
	OriginalPrompt  string            `json:"original_prompt,omitempty"`
	RetrievalState  *retrieval.Record `json:"-"`
	RetrievalRepair bool              `json:"-"`

	// Durable accepts trusted coordinator state only.
	Durable   *DurableExecution `json:"-"`
	Prompt    string            `json:"prompt"`
	Purpose   string            `json:"purpose,omitempty"`
	Dir       string            `json:"dir"`
	AttemptID string            `json:"attempt_id,omitempty"`

	// Options shares configuration across entry points.
	Options

	// RunDir stores API history for continuation.
	RunDir string `json:"-"`

	// Resume seeds API history before Prompt.
	Resume *Conversation `json:"-"`

	// ForkSession preserves the original CLI session.
	ForkSession string `json:"fork_session,omitempty"`

	// FixRound requests one commit for continuation fixes.
	FixRound bool `json:"-"`

	// Approve defaults to denial when nil.
	Approve func(ApprovalRequest) bool `json:"-"`
}

// Conversation is a run's saved history; Model is kept because thinking blocks are bound to it.
type Conversation struct {
	Retrieval *retrieval.Record `json:"retrieval,omitempty"`
	Model     string            `json:"model"`
	Messages  []Message         `json:"messages"`
}

// Message aliases an API turn without changing its blocks.
type Message = apiMessage

type EventKind string

const (
	EventText       EventKind = "text"
	EventThinking   EventKind = "thinking"
	EventToolCall   EventKind = "tool_call"
	EventToolResult EventKind = "tool_result"
	EventInfo       EventKind = "info"
	EventError      EventKind = "error"
	EventSession    EventKind = "session"
	// EventUsage is one model call's token usage, in Event.Usage.
	EventUsage EventKind = "usage"
	// EventCompaction puts its trigger in Text and prior context in Usage.ContextTokens.
	EventCompaction EventKind = "compaction"
	// EventQuota is how much of the plan's quota the backend reported used,
	// in Event.Quota: the numbers only.
	EventQuota EventKind = "quota"
)

// Quota is a plan's usage per window as a backend reported it. A window the
// report did not carry is nil.
type Quota struct {
	FiveHour *QuotaWindow `json:"five_hour,omitempty"`
	Week     *QuotaWindow `json:"week,omitempty"`
}

// QuotaWindow is one window: the percent of it used, 0 to 100, and when it
// resets, in Unix seconds.
type QuotaWindow struct {
	UsedPercent float64 `json:"used_percent"`
	ResetsAt    int64   `json:"resets_at"`
}

// Usage records per-call tokens including cached input.
type Usage struct {
	Model         string `json:"model,omitempty"`
	InputTokens   int64  `json:"input_tokens,omitempty"`
	OutputTokens  int64  `json:"output_tokens,omitempty"`
	CacheRead     int64  `json:"cache_read_tokens,omitempty"`
	CacheWrite    int64  `json:"cache_write_tokens,omitempty"`
	ContextTokens int64  `json:"context_tokens,omitempty"`
}

type Event struct {
	Time    time.Time       `json:"t"`
	Kind    EventKind       `json:"kind"`
	Text    string          `json:"text,omitempty"`
	Tool    string          `json:"tool,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
	// Usage holds per-call counters, unlike Result.Usage.
	Usage *Usage `json:"usage,omitempty"`
	Quota *Quota `json:"quota,omitempty"`

	// Partial fragments are rendered but excluded from transcripts.
	Partial bool `json:"-"`
	// Streamed marks blocks whose fragments were already emitted.
	Streamed bool `json:"-"`
}

type Status string

const (
	StatusOK       Status = "ok"
	StatusError    Status = "error"
	StatusRefused  Status = "refused"
	StatusMaxTurns Status = "max_turns"
	StatusTimeout  Status = "timeout" // the task's own deadline passed
	StatusBudget   Status = "budget"  // a cost or token ceiling was reached
	StatusCanceled Status = "canceled"
	// StatusLimited reports a CLI subscription limit.
	StatusLimited Status = "limited"
)

type Result struct {
	Retrieval *retrieval.Record `json:"retrieval,omitempty"`
	// Usage describes the run-level token and cost totals.
	Usage *UsageMetadata `json:"usage_metadata,omitempty"`
	// RecoveryRequired requires reconciliation before replay.
	RecoveryRequired bool   `json:"recovery_required,omitempty"`
	Backend          string `json:"backend"`
	Model            string `json:"model,omitempty"`
	// ModelReported distinguishes actual and requested model names.
	ModelReported bool   `json:"model_reported,omitempty"`
	Status        Status `json:"status"`
	Final         string `json:"final,omitempty"`
	Error         string `json:"error,omitempty"`
	Turns         int    `json:"turns"`
	InputTokens   int64  `json:"input_tokens,omitempty"`
	OutputTokens  int64  `json:"output_tokens,omitempty"`
	CacheRead     int64  `json:"cache_read_tokens,omitempty"`
	CacheWrite    int64  `json:"cache_write_tokens,omitempty"`
	// ContextTokens includes caches for the last call; totals-only reports omit it.
	ContextTokens int64 `json:"context_tokens,omitempty"`
	// ToolCalls counts emitted calls or durable committed outcomes.
	ToolCalls    int           `json:"tool_calls,omitempty"`
	CostUSD      float64       `json:"cost_usd,omitempty"`
	CostEstimate bool          `json:"cost_is_estimate,omitempty"`
	Duration     time.Duration `json:"duration_ns"`
	// Session permits CLI access and continuation.
	Session string `json:"session,omitempty"`
	// LimitResets is absent when the CLI gives no time.
	LimitResets time.Time `json:"limit_resets,omitzero"`
	// CLIVersion is the version the CLI recorded for this run.
	CLIVersion string `json:"cli_version,omitempty"`
}

// UnmarshalJSON also accepts legacy timeout_ns records.
func (t *Task) UnmarshalJSON(data []byte) error {
	type plain Task // no methods, so this does not recurse
	aux := struct {
		*plain
		TimeoutNS int64 `json:"timeout_ns"`
	}{plain: (*plain)(t)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if t.Timeout == 0 && aux.TimeoutNS != 0 {
		t.Timeout = Duration(aux.TimeoutNS)
	}
	return nil
}

// Backend reports failures in Result.Status rather than Go errors.
type Backend interface {
	Name() string
	// Detect reports the backend source or unavailability.
	Detect() (string, error)
	Run(ctx context.Context, task Task, emit func(Event)) Result
}

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
const MinTaskBudget = 20000

// ValidateLimits rejects unsupported execution limits.
func ValidateLimits(timeout time.Duration, maxCostUSD float64, taskBudget int) error {
	if timeout < 0 {
		return fmt.Errorf("timeout %s is negative", timeout)
	}
	if maxCostUSD < 0 {
		return fmt.Errorf("max cost %.2f is negative", maxCostUSD)
	}
	if taskBudget != 0 && taskBudget < MinTaskBudget {
		return fmt.Errorf("task budget %d is below the API minimum of %d tokens", taskBudget, MinTaskBudget)
	}
	return nil
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
