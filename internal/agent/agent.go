// Package agent defines the contract every backend implements: take a Task,
// stream Events while working, and return a Result. The orchestrator and eval
// layers are built on this contract, so it stays small and serializable.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
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

// ApprovalRequest is raised when a tool call needs a human decision.
type ApprovalRequest struct {
	Tool    string
	Summary string
}

type Task struct {
	Prompt    string `json:"prompt"`
	Dir       string `json:"dir"`
	AttemptID string `json:"attempt_id,omitempty"`

	// Options is everything a caller may set besides the prompt and the
	// workspace: the one struct the run flags, a batch spec and the MCP run
	// tool all fill in. Its fields are promoted, so task.Mode reads as before.
	Options

	// RunDir is where the api backend saves the conversation, so the run can be
	// continued later. Callers that hold a runlog.Writer set it to its Dir.
	RunDir string `json:"-"`

	// Resume is a conversation from an earlier run that this one continues.
	// The api backend seeds its history with it, then appends Prompt.
	Resume *Conversation `json:"-"`

	// ForkSession is the session id an earlier claude or codex run reported.
	// The CLI starts from a copy of that session, so the earlier run's own
	// session stays as it was and the same run can be continued twice.
	ForkSession string `json:"fork_session,omitempty"`

	// Approve decides calls that the mode does not auto-approve. Nil means deny.
	Approve func(ApprovalRequest) bool `json:"-"`
}

// Conversation is the message history of a finished run, saved next to its
// transcript so a follow-up can carry on from it. Thinking blocks are bound to
// the model that produced them, which is why the model is recorded with them.
type Conversation struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
}

// Message is one turn of a Conversation: the role and the content blocks
// exactly as the API returned them. An alias, so other packages can build one.
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
)

type Event struct {
	Time    time.Time       `json:"t"`
	Kind    EventKind       `json:"kind"`
	Text    string          `json:"text,omitempty"`
	Tool    string          `json:"tool,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	IsError bool            `json:"is_error,omitempty"`

	// Partial marks a streaming fragment. Renderers print it; transcripts skip it.
	Partial bool `json:"-"`
	// Streamed marks a complete block whose fragments were already emitted.
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
)

type Result struct {
	Backend string `json:"backend"`
	Model   string `json:"model,omitempty"`
	// ModelReported distinguishes a backend report from a requested/default model.
	ModelReported bool   `json:"model_reported,omitempty"`
	Status        Status `json:"status"`
	Final         string `json:"final,omitempty"`
	Error         string `json:"error,omitempty"`
	Turns         int    `json:"turns"`
	InputTokens   int64  `json:"input_tokens,omitempty"`
	OutputTokens  int64  `json:"output_tokens,omitempty"`
	CacheRead     int64  `json:"cache_read_tokens,omitempty"`
	CacheWrite    int64  `json:"cache_write_tokens,omitempty"`
	// ToolCalls counts the tool calls the agent made. Set by Run, so it is the
	// same count on every backend.
	ToolCalls    int           `json:"tool_calls,omitempty"`
	CostUSD      float64       `json:"cost_usd,omitempty"`
	CostEstimate bool          `json:"cost_is_estimate,omitempty"`
	Duration     time.Duration `json:"duration_ns"`
	// Session is the CLI's own session id (claude, codex), so a follow-up run
	// can fork it, and a person can open it in that CLI.
	Session string `json:"session,omitempty"`
}

// UnmarshalJSON reads a task as written today and as records before 0.3 wrote
// it, when the timeout was a count of nanoseconds under timeout_ns.
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

// Backend runs a task. Run never returns a Go error: failures are reported in
// Result.Status so callers can record and compare every run the same way.
type Backend interface {
	Name() string
	// Detect reports where the backend comes from (binary path, credential source)
	// or why it cannot run.
	Detect() (string, error)
	Run(ctx context.Context, task Task, emit func(Event)) Result
}

// Capabilities describes the task fields a backend can enforce for a supervisor.
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

// Run runs a task on a backend and counts the tool calls it makes. Callers use
// it rather than Backend.Run so the count does not depend on each adapter
// remembering to keep one.
func Run(ctx context.Context, b Backend, task Task, emit func(Event)) Result {
	calls := 0
	res := b.Run(ctx, task, func(e Event) {
		if e.Kind == EventToolCall && !e.Partial {
			calls++
		}
		emit(e)
	})
	res.ToolCalls = calls
	return res
}

func finish(res *Result, started time.Time) Result {
	res.Duration = time.Since(started)
	return *res
}

// Version is reported to MCP peers as the hrn version.
const Version = "0.2.0"

// DefaultBackendName is api when API credentials exist, otherwise claude.
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

// ValidateLimits rejects limits the API or hrn would refuse, where the values
// are typed rather than after an agent has already started.
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

// withTimeout applies the task's own deadline. The backends do this rather
// than their callers, so every caller gets it and every other Task field is
// honoured in the same place.
func withTimeout(ctx context.Context, task Task) (context.Context, context.CancelFunc) {
	if task.Timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, time.Duration(task.Timeout))
}

// stopStatus reports why a context ended: the task's deadline, or a cancel.
func stopStatus(ctx context.Context) Status {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return StatusTimeout
	}
	return StatusCanceled
}

// unenforced names the options a CLI backend cannot apply, for one warning line.
func unenforced(task Task, mapsEffort bool) []string {
	var out []string
	if task.MaxCostUSD > 0 {
		out = append(out, "max_cost")
	}
	if task.TaskBudget > 0 {
		out = append(out, "task_budget")
	}
	if task.Effort != "" && !mapsEffort {
		out = append(out, "effort")
	}
	return out
}
