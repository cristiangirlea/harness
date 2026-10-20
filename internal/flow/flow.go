// Package flow is the versioned execution-event envelope and the step tree built from it.
package flow

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cristiangirlea/harness/internal/mask"
)

// Version is the envelope version every writer stamps; readers accept it and older ones.
const Version = 1

type Kind string

const (
	KindBatch    Kind = "batch"
	KindTask     Kind = "task"
	KindSlot     Kind = "slot"    // waiting for and taking a worktree
	KindAttempt  Kind = "attempt" // one agent run inside a task
	KindRoute    Kind = "route"   // why a backend or model was chosen
	KindDecision Kind = "decision"
	KindRun      Kind = "run"
	KindTool     Kind = "tool"
	KindGate     Kind = "gate" // verification: gates, expect and protect checks
	KindCommit   Kind = "commit"
	KindInfo     Kind = "info"
	KindError    Kind = "error"
)

type Status string

const (
	StatusStarted  Status = "started"
	StatusWaiting  Status = "waiting"
	StatusOK       Status = "ok"
	StatusFailed   Status = "failed"
	StatusCanceled Status = "canceled"
	StatusSkipped  Status = "skipped"
)

// Done reports whether a status ends its step.
func (s Status) Done() bool {
	return s == StatusOK || s == StatusFailed || s == StatusCanceled || s == StatusSkipped
}

// Event is one fact about a step; later events for the same Step update it.
type Event struct {
	V       int       `json:"v"`
	Seq     int64     `json:"seq"`
	Time    time.Time `json:"t"`
	Owner   string    `json:"owner,omitempty"`
	Batch   string    `json:"batch,omitempty"`
	Task    string    `json:"task,omitempty"`
	Attempt int       `json:"attempt,omitempty"`
	Run     string    `json:"run,omitempty"`
	Step    string    `json:"step"`
	Parent  string    `json:"parent,omitempty"`
	Kind    Kind      `json:"kind"`
	Status  Status    `json:"status,omitempty"`
	Name    string    `json:"name,omitempty"`
	Text    string    `json:"text,omitempty"`
	// Reason is a recorded cause (a route row, a gate's exit), never inferred model reasoning.
	Reason string `json:"reason,omitempty"`
	Next   string `json:"next,omitempty"`
	// Derived marks events rebuilt from older records rather than written live.
	Derived bool `json:"derived,omitempty"`
}

const maxText = 2000

// Clean masks secrets and bounds text before an event is stored or shown.
func Clean(e Event) Event {
	e.Name = clip(mask.Text(e.Name), 300)
	e.Text = clip(mask.Text(e.Text), maxText)
	e.Reason = clip(mask.Text(e.Reason), maxText)
	e.Next = clip(mask.Text(e.Next), 500)
	return e
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}

// Writer appends events to a JSONL file with a sequence that continues across reopenings.
type Writer struct {
	mu   sync.Mutex
	f    *os.File
	seq  int64
	base Event
	lost int
	err  error
}

// Open appends to path; base supplies the owner and ids every event carries.
func Open(path string, base Event) (*Writer, error) {
	prior, _ := Read(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	w := &Writer{f: f, base: base}
	if n := len(prior); n > 0 {
		w.seq = prior[n-1].Seq
	}
	return w, nil
}

// Emit records e; a failed write is counted, never fatal to the work it describes.
func (w *Writer) Emit(e Event) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seq++
	e.V, e.Seq = Version, w.seq
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if e.Owner == "" {
		e.Owner = w.base.Owner
	}
	if e.Batch == "" {
		e.Batch = w.base.Batch
	}
	data, err := json.Marshal(Clean(e))
	if err == nil {
		_, err = w.f.Write(append(data, '\n'))
	}
	if err != nil {
		w.lost++
		if w.err == nil {
			w.err = err
		}
	}
}

// Close reports the first lost event, if any.
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return errors.Join(w.err, w.f.Close())
}

// Read returns events in sequence order, dropping duplicates and lines that do not parse.
func Read(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Decode(f)
}

// Decode is Read for any stream; a torn last line from a live writer is skipped.
func Decode(r io.Reader) ([]Event, error) {
	var out []Event
	seen := map[int64]bool{}
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			var e Event
			if json.Unmarshal(line, &e) == nil && e.Step != "" && e.V <= Version && !seen[e.Seq] {
				seen[e.Seq] = true
				out = append(out, e)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, err
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// Renumber gives merged streams one sequence, keeping each stream's order and interleaving by time.
func Renumber(streams ...[]Event) []Event {
	var all []Event
	for _, s := range streams {
		all = append(all, s...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Time.Before(all[j].Time) })
	for i := range all {
		all[i].Seq = int64(i + 1)
	}
	return all
}
