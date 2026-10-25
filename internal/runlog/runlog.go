// Package runlog persists task.json, events.jsonl and result.json per run.
package runlog

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/owner"
	"github.com/cristiangirlea/harness/internal/retrieval"
	"github.com/cristiangirlea/harness/internal/route/table"
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

type RoutedBy = table.RoutedBy

type Record struct {
	RoutedBy  *RoutedBy `json:"routed_by,omitempty"`
	RouteNote string    `json:"route_note,omitempty"`
	ID        string    `json:"id"`
	// Owner is empty on records written before ownership; owner.Of reads that as the local user.
	Owner   string       `json:"owner,omitempty"`
	Backend string       `json:"backend"`
	Started time.Time    `json:"started"`
	Task    agent.Task   `json:"task"`
	Result  agent.Result `json:"result"`

	Continues string `json:"continues,omitempty"`
	// ContinueRoute is empty (default) or fresh_handover (record-briefed session).
	ContinueRoute string `json:"continue_route,omitempty"`
	RecordError   string `json:"record_error,omitempty"`
	Done          bool   `json:"-"`

	Setup *agent.Setup `json:"setup,omitempty"`
}

type Writer struct {
	dir        string
	mu         sync.Mutex
	events     io.WriteCloser
	firstError error
	closeError error
	lostEvents int
	brokenLine bool
	rec        Record
}

// Create starts a run record.
func Create(backend string, task agent.Task) (*Writer, error) {
	return create(backend, task, "", "", nil, "")
}

// CreateContinuing records a continuation.
func CreateContinuing(backend string, task agent.Task, continues string, routed ...*RoutedBy) (*Writer, error) {
	var by *RoutedBy
	if len(routed) > 0 {
		by = routed[0]
	}
	return create(backend, task, continues, "", by, "")
}

// RouteFreshHandover means a new session briefed from the record.
const RouteFreshHandover = "fresh_handover"

// CreateContinuingRoute records a route, empty for default.
func CreateContinuingRoute(backend string, task agent.Task, continues, route string, routed ...*RoutedBy) (*Writer, error) {
	var by *RoutedBy
	if len(routed) > 0 {
		by = routed[0]
	}
	return CreateRouted(backend, task, continues, route, by, "")
}

// CreateRouted records route selection evidence alongside the task.
func CreateRouted(backend string, task agent.Task, continues, route string, routed *RoutedBy, note string) (*Writer, error) {
	return create(backend, task, continues, route, routed, note)
}

var createEventsFile = func(path string) (io.WriteCloser, error) { return os.Create(path) }

// Failed starts must leave no partial record.
func create(backend string, task agent.Task, continues, route string, routed *RoutedBy, note string) (w *Writer, err error) {
	id := NewID()
	dir := filepath.Join(Root(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	w = &Writer{dir: dir, rec: Record{ID: id, Owner: owner.Local(), Backend: backend, Started: time.Now(), Task: task, Continues: continues, ContinueRoute: route,
		Setup: agent.ResolveSetup(backend, task), RouteNote: note}}
	if routed != nil {
		by := *routed
		w.rec.RoutedBy = &by
	}
	if err := writeJSON(filepath.Join(dir, "task.json"), w.rec); err != nil {
		return nil, err
	}
	f, err := createEventsFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return nil, err
	}
	w.events = f
	return w, nil
}

func (w *Writer) ID() string  { return w.rec.ID }
func (w *Writer) Dir() string { return w.dir }

// Event records whole events, skipping fragments.
func (w *Writer) Event(e agent.Event) {
	if e.Partial {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	data, err := json.Marshal(e)
	if err == nil {
		data = append(data, '\n')
		if w.brokenLine {
			// Isolate fragments to preserve the next complete event.
			data = append([]byte{'\n'}, data...)
		}
		var n int
		n, err = w.events.Write(data)
		if err == nil && n < len(data) {
			err = io.ErrShortWrite
		}
		if n > 0 {
			w.brokenLine = data[n-1] != '\n'
		}
	}
	if err != nil {
		if w.firstError == nil {
			w.firstError = err
		}
		w.lostEvents++
	}
}

// Err reports errors safely alongside Event and Close.
func (w *Writer) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.errLocked()
}

// errLocked requires w.mu to be held.
func (w *Writer) errLocked() error {
	var eventErr error
	if w.firstError != nil {
		if w.lostEvents == 1 {
			eventErr = fmt.Errorf("1 event was not recorded: %w", w.firstError)
		} else {
			eventErr = fmt.Errorf("%d events were not recorded: %w", w.lostEvents, w.firstError)
		}
	}
	return errors.Join(eventErr, w.closeError)
}

// ResultWriteError distinguishes unsaved results from lost events.
type ResultWriteError struct {
	Err error
}

func (e *ResultWriteError) Error() string { return e.Err.Error() }
func (e *ResultWriteError) Unwrap() error { return e.Err }

// ResultNotSaved identifies an unsaved result after Close.
func ResultNotSaved(err error) bool {
	var saveErr *ResultWriteError
	return errors.As(err, &saveErr)
}

// CloseMessage distinguishes unsaved results from lost events.
func CloseMessage(err error) string {
	if ResultNotSaved(err) {
		return "saving run: " + err.Error()
	}
	return "run record incomplete: " + err.Error()
}

func (w *Writer) Close(res agent.Result) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closeError = w.events.Close()
	w.rec.Result = res
	recordErr := w.errLocked()
	if recordErr != nil {
		w.rec.RecordError = recordErr.Error()
	}
	err := writeJSON(filepath.Join(w.dir, "result.json"), w.rec)
	if err != nil {
		err = &ResultWriteError{Err: err}
	}
	return errors.Join(recordErr, err)
}

// NewID returns a sortable timestamp with random suffix.
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

// List returns newest runs, capped by positive limit; no result means Done=false.
func List(limit int) ([]Record, error) {
	recs, _, err := list(time.Time{}, limit, nil)
	return recs, err
}

// ListFor is List of the runs v may read; others are skipped before they count toward limit.
func ListFor(v owner.Viewer, limit int) ([]Record, error) {
	recs, _, err := list(time.Time{}, limit, func(r Record) bool { return v.CanRead(r.Owner) })
	return recs, err
}

// LoadFor is Load behind the read decision; a run v may not read is reported as missing.
func LoadFor(v owner.Viewer, id string) (Record, error) {
	rec, err := Load(id)
	if err != nil {
		return rec, err
	}
	if !v.CanRead(rec.Owner) {
		return Record{}, fmt.Errorf("run %s: %w", id, os.ErrNotExist)
	}
	return rec, nil
}

// ListSince lists recent runs and unreadable counts with a day of clock slack.
// Ids start with the local start time, so older ones are skipped unopened; callers filter by recorded time.
func ListSince(since time.Time) ([]Record, int, error) {
	return list(since, 0, nil)
}

const idTimeLayout = "20060102-150405"

func list(since time.Time, limit int, keep func(Record) bool) (recs []Record, unreadable int, err error) {
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
		if keep != nil && !keep(rec) {
			continue
		}
		recs = append(recs, rec)
		if limit > 0 && len(recs) == limit {
			break
		}
	}
	return recs, unreadable, nil
}

func idBefore(id string, t time.Time) bool {
	if t.IsZero() || len(id) < len(idTimeLayout) {
		return false
	}
	ts, err := time.ParseInLocation(idTimeLayout, id[:len(idTimeLayout)], time.Local)
	return err == nil && ts.Before(t)
}

// Reject run-directory traversal.
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
		if err := json.Unmarshal(data, &rec); err != nil {
			return rec, err
		}
		rec.ID = id
		return rec, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return rec, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "task.json"))
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, err
	}
	rec.ID = id
	return rec, nil
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
	r := bufio.NewReader(f)
	checked, recoverBroken := false, false
	for {
		line, readErr := r.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return out, readErr
		}
		if line = bytes.TrimSpace(line); len(line) != 0 {
			var e agent.Event
			if err := json.Unmarshal(line, &e); err != nil {
				if !checked {
					// Skip broken lines only after a saved recording failure.
					rec, loadErr := Load(id)
					recoverBroken, checked = loadErr == nil && rec.RecordError != "", true
				}
				if !recoverBroken {
					return out, err
				}
			} else {
				out = append(out, e)
			}
		}
		if readErr == io.EOF {
			return out, nil
		}
	}
}

// ErrNoConversation marks a record with no saved API history.
var ErrNoConversation = errors.New("no saved conversation")

// LoadConversation returns a record and saved API history.
func LoadConversation(id string) (Record, agent.Conversation, error) {
	var conv agent.Conversation
	rec, err := Load(id)
	if err != nil {
		return rec, conv, err
	}
	data, err := os.ReadFile(filepath.Join(Root(), id, "messages.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return rec, conv, fmt.Errorf("run %s has no saved conversation: only api runs save one: %w", id, ErrNoConversation)
		}
		return rec, conv, err
	}
	if err := json.Unmarshal(data, &conv); err != nil {
		return rec, conv, err
	}
	// Missing results must not discard pre-execution pins.
	data, err = os.ReadFile(filepath.Join(Root(), id, "retrieval.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return rec, conv, err
	}
	if err == nil {
		var saved *retrieval.Record
		if err := json.Unmarshal(data, &saved); err != nil {
			return rec, conv, fmt.Errorf("invalid persisted retrieval: %w", err)
		}
		if saved == nil {
			return rec, conv, fmt.Errorf("empty persisted retrieval")
		}
		if err := agent.ValidateContinuationPin(saved, &conv); err != nil {
			return rec, conv, err
		}
		if !rec.Done {
			rec.Result.Retrieval = saved
		}
	} else if rec.Result.Retrieval != nil || conv.Retrieval != nil || (rec.Task.Retrieval != "" && rec.Task.Retrieval != "off") {
		return rec, conv, fmt.Errorf("persisted retrieval metadata missing")
	}
	if r := conv.Retrieval; r != nil {
		if rec.Done && rec.Result.Model != "" && rec.Result.Model != conv.Model {
			return rec, conv, fmt.Errorf("saved conversation differs from reported model")
		}
		o := rec.Task.Options
		original := rec.Task.OriginalPrompt
		if original == "" {
			original = rec.Task.Prompt
		}
		if r.OriginalPrompt != original {
			return rec, conv, fmt.Errorf("saved retrieval original task mismatch")
		}
		if rec.Backend != "api" || (o.Model != "" && o.Model != r.Scope.Model) || (o.Retrieval != "" && o.Retrieval != r.Mode) || (o.RetrievalTenant != "" && o.RetrievalTenant != r.Scope.TenantID) || (o.RetrievalProject != "" && o.RetrievalProject != r.Scope.ProjectID) || (o.RetrievalRevision != "" && o.RetrievalRevision != r.Scope.Revision) || (o.RetrievalVersion != "" && (r.Packet == nil || o.RetrievalVersion != r.Packet.VersionID)) {
			return rec, conv, fmt.Errorf("saved task differs from retrieval scope or mode")
		}
	}
	return rec, conv, agent.ValidateContinuationPin(rec.Result.Retrieval, &conv)
}

// Chain returns runs oldest first and names any missing predecessor.
func Chain(id string) (chain []Record, missing string, err error) {
	seen := map[string]bool{}
	for next := id; next != ""; {
		if seen[next] {
			return nil, "", fmt.Errorf("run %s: its continue chain loops at %s", id, next)
		}
		seen[next] = true
		rec, err := Load(next)
		if err != nil {
			if next == id {
				return nil, "", err
			}
			if !errors.Is(err, os.ErrNotExist) {
				// Refuse broken history rather than silently dropping it.
				return nil, "", fmt.Errorf("run %s continues %s, which cannot be read: %w", id, next, err)
			}
			missing = next
			break
		}
		chain = append(chain, rec)
		next = rec.Continues
	}
	slices.Reverse(chain)
	return chain, missing, nil
}
