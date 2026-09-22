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
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
)

// Home is $HRN_HOME, or ~/.hrn.
func Home() string {
	if h := os.Getenv("HRN_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".hrn")
}

// Root is the runs directory under Home.
func Root() string { return filepath.Join(Home(), "runs") }

type Record struct {
	ID      string       `json:"id"`
	Backend string       `json:"backend"`
	Started time.Time    `json:"started"`
	Task    agent.Task   `json:"task"`
	Result  agent.Result `json:"result"`
	// Continues names the run this one follows, when it was started with
	// hrn run --continue.
	Continues string `json:"continues,omitempty"`
	Done      bool   `json:"-"`
}

type Writer struct {
	dir    string
	mu     sync.Mutex
	events *os.File
	enc    *json.Encoder
	rec    Record
}

// Create starts a run record.
func Create(backend string, task agent.Task) (*Writer, error) {
	return create(backend, task, "")
}

// CreateContinuing starts a run record that follows an earlier run.
func CreateContinuing(backend string, task agent.Task, continues string) (*Writer, error) {
	return create(backend, task, continues)
}

func create(backend string, task agent.Task, continues string) (*Writer, error) {
	id := NewID()
	dir := filepath.Join(Root(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	w := &Writer{dir: dir, rec: Record{ID: id, Backend: backend, Started: time.Now(), Task: task, Continues: continues}}
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

// NewID is a sortable timestamp plus random suffix, shared by runs and batches.
func NewID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return time.Now().Format(idTimeLayout) + "-" + hex.EncodeToString(b)
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// List returns runs newest first, at most limit of them when limit is
// positive. Runs without result.json are still running or were killed; they
// come back with Done=false.
func List(limit int) ([]Record, error) {
	recs, _, err := list(time.Time{}, limit)
	return recs, err
}

// ListSince returns the runs started at or after since, newest first, and how
// many records could not be read. Run ids begin with the local start time, so
// directories older than the window are skipped without being opened. The
// cut-off keeps a day of slack because the id is only as exact as the clock
// that made it; callers filter by the recorded time anyway.
func ListSince(since time.Time) ([]Record, int, error) {
	return list(since, 0)
}

const idTimeLayout = "20060102-150405"

func list(since time.Time, limit int) (recs []Record, unreadable int, err error) {
	entries, err := os.ReadDir(Root())
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })
	var cutoff time.Time
	if !since.IsZero() {
		cutoff = since.Add(-24 * time.Hour)
	}
	for _, e := range entries {
		if !e.IsDir() || idBefore(e.Name(), cutoff) {
			continue
		}
		rec, err := Load(e.Name())
		if err != nil {
			unreadable++
			continue
		}
		recs = append(recs, rec)
		if limit > 0 && len(recs) == limit {
			break
		}
	}
	return recs, unreadable, nil
}

// idBefore reports whether a run id's time prefix is before t. The zero t and
// an id without a readable prefix never match.
func idBefore(id string, t time.Time) bool {
	if t.IsZero() || len(id) < len(idTimeLayout) {
		return false
	}
	ts, err := time.ParseInLocation(idTimeLayout, id[:len(idTimeLayout)], time.Local)
	return err == nil && ts.Before(t)
}

// validID refuses an id that would reach outside the runs directory.
func validID(id string) error {
	if id == "" || strings.ContainsAny(id, `/\`) || id == ".." {
		return fmt.Errorf("invalid run id %q", id)
	}
	return nil
}

func Load(id string) (Record, error) {
	if err := validID(id); err != nil {
		return Record{}, err
	}
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
	if err := validID(id); err != nil {
		return nil, err
	}
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

// LoadConversation returns a run's record together with the message history it
// saved, for continuing it. Only the api backend saves one.
func LoadConversation(id string) (Record, agent.Conversation, error) {
	var conv agent.Conversation
	rec, err := Load(id)
	if err != nil {
		return rec, conv, err
	}
	data, err := os.ReadFile(filepath.Join(Root(), id, "messages.json"))
	if err != nil {
		return rec, conv, fmt.Errorf("run %s has no saved conversation: only api runs save one", id)
	}
	return rec, conv, json.Unmarshal(data, &conv)
}
