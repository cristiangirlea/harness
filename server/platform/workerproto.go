package platform

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

type workerOutcome string

const (
	outcomeFailed   workerOutcome = "failed"
	outcomeSaved    workerOutcome = "saved"
	outcomeRecovery workerOutcome = "recovery-required"
	outcomeRefused  workerOutcome = "refused"
	outcomeCanceled workerOutcome = "canceled"
)

type checkpointDecision int

const (
	checkpointRejected checkpointDecision = iota
	checkpointSaved
	checkpointNoAnswer
)

type workerClient struct {
	result              JobResult
	input               io.Reader
	output              io.Writer
	attemptID           string
	start               json.RawMessage
	checkpoint          func(json.RawMessage) checkpointDecision
	event               func(json.RawMessage)
	mu                  sync.Mutex
	started             bool
	canceledBeforeStart bool
}

func (c *workerClient) write(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	enc := json.NewEncoder(c.output)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
func (c *workerClient) cancel() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started {
		c.canceledBeforeStart = true
		if closer, ok := c.output.(io.Closer); ok {
			return closer.Close()
		}
		return nil
	}
	return json.NewEncoder(c.output).Encode(map[string]any{"type": "cancel", "attempt_id": c.attemptID})
}

func (c *workerClient) run() (workerOutcome, error) {
	s := bufio.NewScanner(c.input)
	s.Buffer(make([]byte, 4096), 16<<20)
	seq := 0
	state := "hello"
	failedSave := false
	for s.Scan() {
		var f struct {
			Version    int             `json:"v"`
			Seq        int             `json:"seq"`
			Type       string          `json:"type"`
			AttemptID  string          `json:"attempt_id"`
			Protocol   string          `json:"protocol"`
			Checkpoint json.RawMessage `json:"checkpoint"`
			Event      json.RawMessage `json:"event"`
			Result     JobResult       `json:"result"`
			Error      struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(s.Bytes(), &f); err != nil {
			return outcomeRecovery, err
		}
		seq++
		if f.Version != 1 || f.Seq != seq {
			return outcomeRecovery, fmt.Errorf("invalid worker output sequence")
		}
		if state == "hello" {
			if f.Type != "hello" || (f.Protocol != "" && f.Protocol != "v1") {
				return outcomeRecovery, fmt.Errorf("expected worker hello")
			}
			c.mu.Lock()
			if c.canceledBeforeStart {
				c.mu.Unlock()
				return outcomeCanceled, nil
			}
			_, err := c.output.Write(append(bytes.Clone(c.start), '\n'))
			if err == nil {
				c.started = true
			}
			c.mu.Unlock()
			if err != nil {
				return outcomeRecovery, err
			}
			state = "start"
			continue
		}
		if f.AttemptID != c.attemptID && !(f.Type == "rejected" && f.AttemptID == "") {
			return outcomeRecovery, fmt.Errorf("worker attempt mismatch")
		}
		if state == "start" {
			if f.Type == "rejected" {
				c.result = JobResult{Backend: "api", Status: StatusError, Error: fmt.Sprintf("refused: %s: %s", f.Error.Code, f.Error.Message)}
				return outcomeRefused, nil
			}
			if f.Type != "accepted" {
				return outcomeRecovery, fmt.Errorf("expected worker acceptance")
			}
			state = "running"
			continue
		}
		switch f.Type {
		case "event":
			if c.event != nil {
				c.event(f.Event)
			}
		case "error":
		case "checkpoint":
			var cp struct {
				Sequence int64 `json:"sequence"`
			}
			if err := json.Unmarshal(f.Checkpoint, &cp); err != nil || cp.Sequence <= 0 || c.checkpoint == nil {
				return outcomeRecovery, fmt.Errorf("invalid checkpoint")
			}
			decision := c.checkpoint(f.Checkpoint)
			if decision != checkpointSaved {
				failedSave = true
			}
			if decision == checkpointNoAnswer {
				continue
			}
			kind := "nack"
			if decision == checkpointSaved {
				kind = "ack"
			}
			if err := c.write(map[string]any{"type": kind, "attempt_id": c.attemptID, "sequence": cp.Sequence}); err != nil {
				return outcomeRecovery, err
			}
		case "result":
			c.result = f.Result
			if f.Result.RecoveryRequired || failedSave {
				return outcomeRecovery, nil
			}
			switch f.Result.Status {
			case "ok":
				return outcomeSaved, nil
			case "canceled":
				return outcomeCanceled, nil
			case "refused":
				return outcomeRefused, nil
			case StatusError, StatusBudget, StatusMaxTurns, StatusTimeout, StatusLimited:
				return outcomeFailed, nil
			default:
				return outcomeRecovery, fmt.Errorf("worker result status %q", f.Result.Status)
			}
		default:
			return outcomeRecovery, fmt.Errorf("unexpected worker frame %q", f.Type)
		}
	}
	if err := s.Err(); err != nil {
		return outcomeRecovery, err
	}
	return outcomeRecovery, io.ErrUnexpectedEOF
}

func workerStart(job Job, attemptID, dir string, checkpoint *Checkpoint) (json.RawMessage, error) {
	task := map[string]any{"prompt": job.Prompt, "dir": dir, "backend": job.Backend, "mode": job.Options.Mode}
	if job.Options.Model != "" {
		task["model"] = job.Options.Model
	}
	if job.Options.Effort != "" {
		task["effort"] = job.Options.Effort
	}
	if job.Options.Timeout != 0 {
		task["timeout"] = job.Options.Timeout
	}
	if job.Options.MaxTurns != 0 {
		task["max_turns"] = job.Options.MaxTurns
	}
	if job.Options.MaxCostUSD != 0 {
		task["max_cost_usd"] = job.Options.MaxCostUSD
	}
	if job.Options.TaskBudget != 0 {
		task["task_budget"] = job.Options.TaskBudget
	}
	frame := map[string]any{"type": "start", "attempt_id": attemptID, "task": task}
	if checkpoint != nil {
		frame["checkpoint"] = nil
	}
	encoded, err := json.Marshal(frame)
	if err != nil || checkpoint == nil {
		return encoded, err
	}
	if len(checkpoint.Bytes()) == 0 {
		return nil, fmt.Errorf("checkpoint has no original payload")
	}
	return bytes.Replace(encoded, []byte(`"checkpoint":null`), append([]byte(`"checkpoint":`), checkpoint.Bytes()...), 1), nil
}
