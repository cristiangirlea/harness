package platform

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cristiangirlea/harness/internal/retrieval"
)

type JobStatus string

const (
	StatusOK       JobStatus = "ok"
	StatusError    JobStatus = "error"
	StatusRefused  JobStatus = "refused"
	StatusMaxTurns JobStatus = "max_turns"
	StatusTimeout  JobStatus = "timeout"
	StatusBudget   JobStatus = "budget"
	StatusCanceled JobStatus = "canceled"
	StatusLimited  JobStatus = "limited"
)

type ResultEvidence struct {
	Kind    string `json:"kind,omitempty"`
	Source  string `json:"source,omitempty"`
	Hash    string `json:"hash,omitempty"`
	Excerpt string `json:"excerpt,omitempty"`
}

type ResultUsage struct {
	InputSemantics string `json:"input_semantics"`
	Status         string `json:"status"`
	CostStatus     string `json:"cost_status,omitempty"`
	Calls          *int   `json:"calls,omitempty"`
}

type JobResult struct {
	Evidence              []ResultEvidence  `json:"evidence,omitempty"`
	ChangedFiles          []string          `json:"changed_files,omitempty"`
	ChangedFilesTruncated bool              `json:"changed_files_truncated,omitempty"`
	Commits               []string          `json:"commits,omitempty"`
	EndHead               string            `json:"end_head,omitempty"`
	Retrieval             *retrieval.Record `json:"retrieval,omitempty"`
	Usage                 *ResultUsage      `json:"usage_metadata,omitempty"`
	RecoveryRequired      bool              `json:"recovery_required,omitempty"`
	Backend               string            `json:"backend"`
	Model                 string            `json:"model,omitempty"`
	ModelReported         bool              `json:"model_reported,omitempty"`
	Status                JobStatus         `json:"status"`
	Final                 string            `json:"final,omitempty"`
	Error                 string            `json:"error,omitempty"`
	Turns                 int               `json:"turns"`
	InputTokens           int64             `json:"input_tokens,omitempty"`
	OutputTokens          int64             `json:"output_tokens,omitempty"`
	CacheRead             int64             `json:"cache_read_tokens,omitempty"`
	CacheWrite            int64             `json:"cache_write_tokens,omitempty"`
	ContextTokens         int64             `json:"context_tokens,omitempty"`
	ToolCalls             int               `json:"tool_calls,omitempty"`
	CostUSD               float64           `json:"cost_usd,omitempty"`
	CostEstimate          bool              `json:"cost_is_estimate,omitempty"`
	Duration              time.Duration     `json:"duration_ns"`
	Session               string            `json:"session,omitempty"`
	LimitResets           time.Time         `json:"limit_resets,omitzero"`
	CLIVersion            string            `json:"cli_version,omitempty"`
}

func decodeResult(data []byte, allowEmpty, lenient bool) (JobResult, error) {
	type plain JobResult
	var v plain
	d := json.NewDecoder(bytes.NewReader(data))
	if !lenient {
		d.DisallowUnknownFields()
	}
	if err := d.Decode(&v); err != nil {
		return JobResult{}, err
	}
	switch v.Status {
	case StatusOK, StatusError, StatusRefused, StatusMaxTurns, StatusTimeout, StatusBudget, StatusCanceled, StatusLimited:
	case "":
		if !allowEmpty {
			return JobResult{}, errors.New("missing result status")
		}
	default:
		return JobResult{}, fmt.Errorf("unknown result status %q", v.Status)
	}
	return JobResult(v), nil
}

func (r *JobResult) UnmarshalJSON(data []byte) error {
	v, err := decodeResult(data, false, false)
	if err == nil {
		*r = v
	}
	return err
}

type Checkpoint struct {
	Version    int
	Sequence   uint64
	NextTurn   int
	Boundary   string
	TaskDigest string
	Pending    *CheckpointPending
	Finished   bool
	Result     JobResult
	raw        []byte
}

type CheckpointPending struct {
	Kind  string          `json:"kind"`
	ID    string          `json:"id"`
	Tool  string          `json:"tool,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

func (c *Checkpoint) UnmarshalJSON(data []byte) error {
	return c.decode(data, false)
}

func (c *Checkpoint) decode(data []byte, allowZero bool) error {
	return c.decodeWithPolicy(data, allowZero, false)
}

func (c *Checkpoint) decodeWithPolicy(data []byte, allowZero, lenient bool) error {
	if bytes.ContainsAny(data, "\r\n") {
		return errors.New("checkpoint must be on one line")
	}
	if len(data) > 16*1024*1024 {
		return errors.New("checkpoint exceeds 16 MiB")
	}
	var v struct {
		Version      int                `json:"version"`
		Sequence     uint64             `json:"sequence"`
		Boundary     string             `json:"boundary"`
		TaskDigest   string             `json:"task_digest"`
		Started      time.Time          `json:"started"`
		Deadline     time.Time          `json:"deadline"`
		NextTurn     int                `json:"next_turn"`
		Request      json.RawMessage    `json:"request"`
		Betas        []string           `json:"betas"`
		Response     json.RawMessage    `json:"response"`
		InvalidInput map[string]string  `json:"invalid_input"`
		ToolResults  []map[string]any   `json:"tool_results"`
		Pending      *CheckpointPending `json:"pending"`
		Result       json.RawMessage    `json:"result"`
		Finished     bool               `json:"finished"`
	}
	d := json.NewDecoder(bytes.NewReader(data))
	if !lenient {
		d.DisallowUnknownFields()
	}
	if err := d.Decode(&v); err != nil {
		return err
	}
	if !(allowZero && v.Version == 0 && v.Sequence == 0 && v.Boundary == "" && v.TaskDigest == "") && (v.Version != 1 || v.Sequence == 0 || v.Boundary == "" || v.TaskDigest == "") {
		return errors.New("invalid API checkpoint")
	}
	var result JobResult
	if len(v.Result) > 0 && string(v.Result) != "null" {
		var err error
		result, err = decodeResult(v.Result, !v.Finished, lenient)
		if err != nil {
			return err
		}
	} else if v.Finished {
		return errors.New("finished checkpoint missing result")
	}
	*c = Checkpoint{Version: v.Version, Sequence: v.Sequence, NextTurn: v.NextTurn, Boundary: v.Boundary, TaskDigest: v.TaskDigest, Pending: v.Pending, Finished: v.Finished, Result: result, raw: bytes.Clone(data)}
	return nil
}

// Bytes returns the original checkpoint payload without JSON normalization.
func (c Checkpoint) Bytes() []byte { return bytes.Clone(c.raw) }

func (c Checkpoint) MarshalJSON() ([]byte, error) {
	if len(c.raw) == 0 {
		return nil, errors.New("checkpoint has no original payload")
	}
	return c.Bytes(), nil
}

func (c *Checkpoint) decodeStored(data []byte) error { return c.decodeWithPolicy(data, false, true) }
