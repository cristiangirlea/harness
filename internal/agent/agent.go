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
	Prompt   string `json:"prompt"`
	Dir      string `json:"dir"`
	Model    string `json:"model,omitempty"`
	Mode     Mode   `json:"mode"`
	MaxTurns int    `json:"max_turns,omitempty"`
	Effort   string `json:"effort,omitempty"`

	// MCP lists config files (absolute paths) whose stdio servers are offered
	// as tools by the api backend and passed through to the claude CLI.
	MCP []string `json:"mcp,omitempty"`

	// NoInstructions skips the project instruction files (HX.md, AGENTS.md,
	// CLAUDE.md) that the api backend otherwise puts in its system prompt.
	NoInstructions bool `json:"no_instructions,omitempty"`

	// Timeout ends the run when it passes; zero means no deadline.
	Timeout time.Duration `json:"timeout_ns,omitempty"`

	// MaxCostUSD stops the api loop before a turn that would take the
	// estimated cost past it. The CLI backends cannot enforce it.
	MaxCostUSD float64 `json:"max_cost_usd,omitempty"`

	// TaskBudget is the api backend's token ceiling for the whole task: the
	// model paces itself and finishes rather than being cut off.
	TaskBudget int `json:"task_budget,omitempty"`

	// NoContextEdits turns off server-side context management, which the api
	// backend asks for by default so old tool results are cleared server-side
	// instead of being resent every turn.
	NoContextEdits bool `json:"no_context_edits,omitempty"`

	// AllowDangerous lifts the api backend's command guard, which otherwise
	// refuses a few command lines (git push, sudo, rm -rf /, curl | sh).
	AllowDangerous bool `json:"allow_dangerous,omitempty"`

	// RunDir is where the api backend saves the conversation, so the run can be
	// continued later. Callers that hold a runlog.Writer set it to its Dir.
	RunDir string `json:"-"`

	// Resume is a conversation from an earlier run that this one continues.
	// The api backend seeds its history with it, then appends Prompt.
	Resume *Conversation `json:"-"`

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
	Backend      string        `json:"backend"`
	Model        string        `json:"model,omitempty"`
	Status       Status        `json:"status"`
	Final        string        `json:"final,omitempty"`
	Error        string        `json:"error,omitempty"`
	Turns        int           `json:"turns"`
	InputTokens  int64         `json:"input_tokens,omitempty"`
	OutputTokens int64         `json:"output_tokens,omitempty"`
	CacheRead    int64         `json:"cache_read_tokens,omitempty"`
	CacheWrite   int64         `json:"cache_write_tokens,omitempty"`
	CostUSD      float64       `json:"cost_usd,omitempty"`
	CostEstimate bool          `json:"cost_is_estimate,omitempty"`
	Duration     time.Duration `json:"duration_ns"`
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

func finish(res *Result, started time.Time) Result {
	res.Duration = time.Since(started)
	return *res
}

// Version is reported to MCP peers as the hx version.
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

// ValidateLimits rejects limits the API or hx would refuse, where the values
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
	return context.WithTimeout(ctx, task.Timeout)
}

// stopStatus reports why a context ended: the task's deadline, or a cancel.
func stopStatus(ctx context.Context) Status {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return StatusTimeout
	}
	return StatusCanceled
}

// unenforced names the limits a backend cannot apply, for one warning line.
func unenforced(task Task) []string {
	var out []string
	if task.MaxCostUSD > 0 {
		out = append(out, "max_cost")
	}
	if task.TaskBudget > 0 {
		out = append(out, "task_budget")
	}
	return out
}
