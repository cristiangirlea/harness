// Package agent defines the contract every backend implements: take a Task,
// stream Events while working, and return a Result. The orchestrator and eval
// layers are built on this contract, so it stays small and serializable.
package agent

import (
	"context"
	"encoding/json"
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

	// Approve decides calls that the mode does not auto-approve. Nil means deny.
	Approve func(ApprovalRequest) bool `json:"-"`
}

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
