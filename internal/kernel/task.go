package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/cristiangirlea/harness/internal/retrieval"
)

type Task struct {
	NoThinking bool                               `json:"no_thinking,omitempty"`
	NoTools    bool                               `json:"no_tools,omitempty"`
	MaxTokens  int                                `json:"max_tokens,omitempty"`
	Knowledge  func() (retrieval.Searcher, error) `json:"-"`

	ChildEnv        []string          `json:"-"`
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

type Message struct {
	Role    string           `json:"role"`
	Content []map[string]any `json:"content"`
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

// DurableExecution's credentials must never reach execution tools; Resume recovers the same task without a new turn.
type DurableExecution struct {
	Resume *APICheckpoint
	Save   func(context.Context, APICheckpoint) error
}

// APICheckpoint preserves signed requests and confirmed outcomes to prevent replay.
// Its untrusted content must stay project-scoped.
type APICheckpoint struct {
	Version      int               `json:"version"`
	Sequence     uint64            `json:"sequence"`
	Boundary     string            `json:"boundary"`
	TaskDigest   string            `json:"task_digest"`
	Started      time.Time         `json:"started"`
	Deadline     time.Time         `json:"deadline,omitempty"`
	NextTurn     int               `json:"next_turn"`
	Request      json.RawMessage   `json:"request"`
	Betas        []string          `json:"betas,omitempty"`
	Response     json.RawMessage   `json:"response,omitempty"`
	InvalidInput map[string]string `json:"invalid_input,omitempty"`
	ToolResults  []map[string]any  `json:"tool_results,omitempty"`
	Pending      *PendingOperation `json:"pending,omitempty"`
	Result       Result            `json:"result"`
	Finished     bool              `json:"finished,omitempty"`

	CompactionK             int    `json:"compaction_k,omitempty"`
	CompactionSummary       string `json:"compaction_summary,omitempty"`
	CompactionFailedContext int64  `json:"compaction_failed_context,omitempty"`
}

type PendingOperation struct {
	Kind  string          `json:"kind"`
	ID    string          `json:"id"`
	Tool  string          `json:"tool,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// ErrRecoveryRequired requires executor termination and effect reconciliation before resubmission.
var ErrRecoveryRequired = errors.New("execution requires recovery")
