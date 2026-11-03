package kernel

import (
	"encoding/json"
	"fmt"
	"strconv"
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

// ApprovalRequest asks for a human tool-call decision.
type ApprovalRequest struct {
	Tool    string
	Summary string
}

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
	Evidence *Evidence       `json:"evidence,omitempty"`
	Time     time.Time       `json:"t"`
	Kind     EventKind       `json:"kind"`
	Text     string          `json:"text,omitempty"`
	Tool     string          `json:"tool,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
	IsError  bool            `json:"is_error,omitempty"`
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

// EffortLevels contains case-sensitive effort names.
var EffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// Modes lists the permission modes, for the MCP schema and the flag help.
var Modes = []Mode{ModeReadOnly, ModeAsk, ModeAuto, ModeYolo}

func (m Mode) String() string { return string(m) }

// Set validates Mode as a flag.Value.
func (m *Mode) Set(s string) error {
	v, err := ParseMode(s)
	if err != nil {
		return err
	}
	*m = v
	return nil
}

// Duration stores durations as text to prevent nanosecond mistakes.
type Duration time.Duration

func (d Duration) String() string { return time.Duration(d).String() }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*d = 0 // absent, as a generated spec or call may spell it
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("duration %s: want a string like \"20m\"", data)
	}
	return d.Set(s)
}

// Set parses Duration as a flag.Value.
func (d *Duration) Set(s string) error {
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("%q is not a duration like 20m", s)
	}
	*d = Duration(v)
	return nil
}

// CompactWindow is auto or 100000-1000000; records store the count as a number.
type CompactWindow string

const minCompactWindow, maxCompactWindow = 100000, 1000000

func (w CompactWindow) Check() error {
	if w == "" || w == "auto" {
		return nil
	}
	if n, err := strconv.Atoi(string(w)); err == nil && strconv.Itoa(n) == string(w) && n >= minCompactWindow && n <= maxCompactWindow {
		return nil
	}
	return fmt.Errorf("compact_window %q: want a plain decimal integer from %d to %d (no sign or leading zeros), or \"auto\"", string(w), minCompactWindow, maxCompactWindow)
}

func (w CompactWindow) String() string { return string(w) }

// Set validates CompactWindow as a flag.Value.
func (w *CompactWindow) Set(s string) error {
	if err := CompactWindow(s).Check(); err != nil {
		return err
	}
	*w = CompactWindow(s)
	return nil
}

func (w CompactWindow) MarshalJSON() ([]byte, error) {
	if w == "auto" {
		return json.Marshal("auto")
	}
	return []byte(w), nil
}

func (w *CompactWindow) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*w = ""
		return nil
	}
	var n int
	if err := json.Unmarshal(data, &n); err == nil {
		*w = CompactWindow(strconv.Itoa(n))
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil || s == "" {
		return fmt.Errorf("compact_window %s: want an integer from %d to %d, or \"auto\"", data, minCompactWindow, maxCompactWindow)
	}
	return w.Set(s)
}

// UsageMetadata describes run totals, inferred only for known adapters.
type UsageMetadata struct {
	InputSemantics string `json:"input_semantics"`       // inclusive_cache or exclusive_cache
	Status         string `json:"status"`                // reported, estimated, missing
	CostStatus     string `json:"cost_status,omitempty"` // reported, estimated, missing
	Calls          *int   `json:"calls,omitempty"`
}
