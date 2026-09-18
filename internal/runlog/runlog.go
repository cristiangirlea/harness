// Package runlog records every run to disk so runs can be listed, replayed and
// compared later (the eval and orchestrator layers read these records).
//
// Layout: <root>/<run-id>/task.json, events.jsonl, result.json
package runlog

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
)

// Root is ~/.hx/runs unless HX_HOME is set.
func Root() string {
	if h := os.Getenv("HX_HOME"); h != "" {
		return filepath.Join(h, "runs")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".hx", "runs")
}

type Record struct {
	ID      string       `json:"id"`
	Backend string       `json:"backend"`
	Started time.Time    `json:"started"`
	Task    agent.Task   `json:"task"`
	Result  agent.Result `json:"result"`
	Done    bool         `json:"-"`
}

type Writer struct {
	dir    string
	mu     sync.Mutex
	events *os.File
	enc    *json.Encoder
	rec    Record
}

func Create(backend string, task agent.Task) (*Writer, error) {
	id := newID()
	dir := filepath.Join(Root(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	w := &Writer{dir: dir, rec: Record{ID: id, Backend: backend, Started: time.Now(), Task: task}}
	if err := writeJSON(filepath.Join(dir, "task.json"), w.rec); err != nil {
		return nil, err
	}
	f, err := os.Create(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return nil, err
	}
	w.events, w.enc = f, json.NewEncoder(f)
	return w, nil
}

func (w *Writer) ID() string  { return w.rec.ID }
func (w *Writer) Dir() string { return w.dir }

// Event appends a complete event; streaming fragments are skipped.
func (w *Writer) Event(e agent.Event) {
	if e.Partial {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.enc.Encode(e)
}

func (w *Writer) Close(res agent.Result) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rec.Result = res
	err := w.events.Close()
	return errors.Join(err, writeJSON(filepath.Join(w.dir, "result.json"), w.rec))
}

func newID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// List returns runs newest first. Runs without result.json are still running
// or were killed; they come back with Done=false.
func List(limit int) ([]Record, error) {
	entries, err := os.ReadDir(Root())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })
	var out []Record
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		rec, err := Load(e.Name())
		if err != nil {
			continue
		}
		out = append(out, rec)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

func Load(id string) (Record, error) {
	dir := filepath.Join(Root(), id)
	var rec Record
	if data, err := os.ReadFile(filepath.Join(dir, "result.json")); err == nil {
		rec.Done = true
		return rec, json.Unmarshal(data, &rec)
	}
	data, err := os.ReadFile(filepath.Join(dir, "task.json"))
	if err != nil {
		return rec, err
	}
	return rec, json.Unmarshal(data, &rec)
}

func Events(id string) ([]agent.Event, error) {
	f, err := os.Open(filepath.Join(Root(), id, "events.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []agent.Event
	dec := json.NewDecoder(f)
	for dec.More() {
		var e agent.Event
		if err := dec.Decode(&e); err != nil {
			return out, err
		}
		out = append(out, e)
	}
	return out, nil
}
