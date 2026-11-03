package platform

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

type workerOutcome string

const (
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
	input      io.Reader
	output     io.Writer
	attemptID  string
	start      json.RawMessage
	checkpoint func(json.RawMessage) checkpointDecision
	event      func(json.RawMessage)
	mu         sync.Mutex
}

func (c *workerClient) write(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	enc := json.NewEncoder(c.output)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
func (c *workerClient) cancel() error {
	return c.write(map[string]any{"type": "cancel", "attempt_id": c.attemptID})
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
			Result     struct {
				Status   string `json:"status"`
				Recovery bool   `json:"recovery_required"`
			} `json:"result"`
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
			if err := c.write(c.start); err != nil {
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
			if f.Result.Recovery || failedSave {
				return outcomeRecovery, nil
			}
			switch f.Result.Status {
			case "ok":
				return outcomeSaved, nil
			case "canceled":
				return outcomeCanceled, nil
			case "refused":
				return outcomeRefused, nil
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
