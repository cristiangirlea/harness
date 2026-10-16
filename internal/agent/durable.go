package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cristiangirlea/harness/internal/tools"
)

// DurableExecution supplies a trusted persistence boundary for the API loop.
// Save must commit before returning success, reject stale owners and preserve
// immutable versions. It must not share its credentials with execution tools.
// Resume is crash recovery of this same task, not a new conversational turn.
type DurableExecution struct {
	Resume *APICheckpoint
	Save   func(context.Context, APICheckpoint) error
}

// APICheckpoint preserves the exact request, including signed assistant blocks,
// and the outcomes needed to continue without repeating confirmed operations.
// It contains untrusted workspace/model content and must stay project-scoped.
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
}

type PendingOperation struct {
	Kind  string          `json:"kind"`
	ID    string          `json:"id"`
	Tool  string          `json:"tool,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// ErrRecoveryRequired means an operation may have happened. The caller must
// establish executor termination and reconcile the effect before resubmitting.
var ErrRecoveryRequired = errors.New("execution requires recovery")

type durableAPI struct {
	checkpoint APICheckpoint
	save       func(context.Context, APICheckpoint) error
}

func prepareDurable(task Task, req *apiRequest, res *Result) (*durableAPI, error) {
	if task.Durable == nil {
		return nil, nil
	}
	if task.Durable.Save == nil {
		return nil, errors.New("durable execution needs a persistence callback")
	}
	if task.Resume != nil {
		return nil, errors.New("conversation continuation and durable recovery cannot be combined")
	}
	identity, err := json.Marshal(task)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(identity)
	cp := APICheckpoint{Version: 1, TaskDigest: hex.EncodeToString(digest[:]), Started: time.Now().UTC(), NextTurn: 1}
	if task.Timeout > 0 {
		cp.Deadline = cp.Started.Add(time.Duration(task.Timeout))
	}
	if saved := task.Durable.Resume; saved != nil {
		// Clone at the trust boundary: a caller cannot mutate an active run's
		// snapshot, and Save never receives pointers to mutable loop state.
		data, err := json.Marshal(saved)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &cp); err != nil {
			return nil, err
		}
		if cp.Version != 1 || cp.TaskDigest != hex.EncodeToString(digest[:]) || cp.NextTurn < 1 || cp.Sequence == 0 || cp.Started.IsZero() {
			return nil, errors.New("checkpoint version or task identity does not match")
		}
		if cp.Pending != nil {
			return nil, fmt.Errorf("%w: unresolved %s operation %s", ErrRecoveryRequired, cp.Pending.Kind, cp.Pending.ID)
		}
		if err := json.Unmarshal(cp.Request, req); err != nil {
			return nil, fmt.Errorf("checkpoint request: %w", err)
		}
		req.betas = append([]string(nil), cp.Betas...)
		*res = cp.Result
	}
	return &durableAPI{checkpoint: cp, save: task.Durable.Save}, nil
}

func (d *durableAPI) persist(ctx context.Context, boundary string, req apiRequest, res Result) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	d.checkpoint.Request = data
	d.checkpoint.Betas = append([]string(nil), req.betas...)
	res.Duration = time.Since(d.checkpoint.Started)
	d.checkpoint.Result = res
	d.checkpoint.Sequence++
	d.checkpoint.Boundary = boundary
	data, err = json.Marshal(d.checkpoint)
	if err != nil {
		return err
	}
	var snapshot APICheckpoint
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return err
	}
	if err := d.save(ctx, snapshot); err != nil {
		// A failed save may still have committed. Never continue or reuse this
		// writer after an ambiguous acknowledgement.
		return fmt.Errorf("%w: persist %s: %v", ErrRecoveryRequired, boundary, err)
	}
	return nil
}

func (d *durableAPI) response() (*apiResponse, error) {
	var response apiResponse
	if err := json.Unmarshal(d.checkpoint.Response, &response); err != nil {
		return nil, err
	}
	response.invalidInput = d.checkpoint.InvalidInput
	return &response, nil
}

func (d *durableAPI) runTools(ctx context.Context, ws *tools.Workspace, task Task, calls []block, invalid map[string]string, byName map[string]tools.Tool, emit func(Event), req apiRequest, res *Result) ([]block, error) {
	if len(d.checkpoint.ToolResults) > len(calls) {
		return nil, errors.New("checkpoint has excess tool outcomes")
	}
	seen := make(map[string]bool, len(calls))
	for _, call := range calls {
		id := str(call["id"])
		if id == "" || seen[id] {
			return nil, errors.New("tool operation IDs must be nonempty and unique within a response")
		}
		seen[id] = true
	}
	// Durable operations run in order. A later operation must never start after
	// an earlier outcome could not be committed. Local execution keeps its
	// existing parallel read behavior.
	for i, call := range calls {
		name, id := str(call["name"]), str(call["id"])
		if i < len(d.checkpoint.ToolResults) {
			if d.checkpoint.ToolResults[i]["tool_use_id"] != id {
				return nil, errors.New("checkpoint tool outcome ID mismatch")
			}
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		input, err := json.Marshal(call["input"])
		if err != nil {
			return nil, err
		}
		tool, ok := byName[name]
		summary := name
		if ok && tool.Summary != nil {
			summary = tool.Summary(input)
		}
		var out string
		var runErr error
		switch {
		case !ok:
			runErr = fmt.Errorf("unknown tool %q", name)
		case invalid[id] != "":
			runErr = fmt.Errorf("tool input was not valid JSON: %s", invalid[id])
		case refusalOf(tool, ws, input) != "":
			runErr = tools.GuardError(refusalOf(tool, ws, input))
		case !allowed(task, tool, summary):
			runErr = errors.New("Permission denied by the user's harness settings.")
		default:
			d.checkpoint.Pending = &PendingOperation{Kind: "tool", ID: id, Tool: name, Input: input}
			if err := d.persist(ctx, "tool_intent", req, *res); err != nil {
				return nil, err
			}
		}
		emit(Event{Time: time.Now(), Kind: EventToolCall, Tool: name, Input: input, Text: summary})
		if runErr == nil {
			out, runErr = tool.Run(ctx, ws, input)
		}
		if runErr != nil {
			if out != "" {
				out += "\n"
			}
			out += runErr.Error()
		}
		outcome := block{"type": "tool_result", "tool_use_id": id, "content": out}
		if runErr != nil {
			outcome["is_error"] = true
		}
		d.checkpoint.ToolResults = append(d.checkpoint.ToolResults, outcome)
		d.checkpoint.Pending = nil
		res.ToolCalls++
		if err := d.persist(ctx, "tool_outcome", req, *res); err != nil {
			return nil, err
		}
		emit(Event{Time: time.Now(), Kind: EventToolResult, Tool: name, Text: out, IsError: runErr != nil})
	}
	return d.checkpoint.ToolResults, nil
}
