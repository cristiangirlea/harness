// Package execproto serves one recorded run over the supervisor JSONL protocol.
package execproto

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/followup"
	"github.com/cristiangirlea/harness/internal/mcp"
	"github.com/cristiangirlea/harness/internal/purpose"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/tools"
)

const (
	MaxInputLine = 4 << 20
	MaxEventText = 256 << 10
)

type record interface {
	ID() string
	Dir() string
	Event(agent.Event)
	Err() error
	Close(agent.Result) error
}
type recordFactory func(string, agent.Task, string, string, *runlog.RoutedBy, string) (record, error)

// Run writes hello before reading input, runs at most one task, and returns the
// protocol exit code. ctx carries interrupts; nil now uses the real clock.
func Run(ctx context.Context, in io.Reader, out io.Writer, now func() time.Time) int {
	return run(ctx, in, out, now, func(b string, t agent.Task, id, route string, routed *runlog.RoutedBy, note string) (record, error) {
		return runlog.CreateRouted(b, t, id, route, routed, note)
	})
}

func run(ctx context.Context, in io.Reader, out io.Writer, now func() time.Time, create recordFactory) int {
	if now == nil {
		now = time.Now
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := &writer{out: out, now: now, cancel: cancel}
	if !w.write("hello", hello()) {
		return 1
	}

	// The input reader may block while the backend completes or an interrupt
	// arrives. Do not close a caller-owned reader. The process owns this reader's
	// lifetime; done releases a blocked channel send when the run finishes.
	done := make(chan struct{})
	defer close(done)
	input := readInput(in, done)
	var first inputLine
	select {
	case first = <-input:
	case <-ctx.Done():
		return w.reject("", "bad_request", "interrupted before start")
	}
	if first.err != nil {
		return w.reject("", "bad_request", first.err.Error())
	}
	var req startRequest
	if err := decode(first.data, &req); err != nil {
		return w.reject(req.AttemptID, "bad_request", err.Error())
	}
	if req.Type != "start" {
		return w.reject(req.AttemptID, "bad_request", "the first message must be start")
	}
	if !attemptID.MatchString(req.AttemptID) {
		return w.reject(req.AttemptID, "bad_request", "attempt_id must be 1 to 128 letters, digits, dots, underscores or dashes")
	}
	if req.Task == nil {
		return w.reject(req.AttemptID, "bad_request", "task is required")
	}
	t := req.Task
	if strings.TrimSpace(t.Prompt) == "" || strings.TrimSpace(t.Dir) == "" || t.Mode == "" {
		return w.reject(req.AttemptID, "bad_request", "prompt, dir and mode are required")
	}
	if t.Fresh && t.ContinueRunID == "" {
		return w.reject(req.AttemptID, "bad_request", "fresh needs continue_run_id: it starts a new session handed an earlier run over")
	}
	opts := agent.Options{Model: t.Model, Mode: t.Mode, Effort: t.Effort, Timeout: t.Timeout, MaxTurns: t.MaxTurns, MCP: t.MCP}
	routed, routeNote, err := opts.RouteWithNote(t.Purpose, t.Prompt, &t.Backend, runlog.Home())
	if err != nil {
		return w.reject(req.AttemptID, "bad_request", err.Error())
	}
	if t.Backend == "" {
		t.Backend = agent.DefaultBackendNameIn(runlog.Home())
		if routed == nil {
			routeNote = agent.WithDefault(routeNote, t.Backend)
		}
	}
	t.Model, t.Effort = opts.Model, opts.Effort
	if t.Model != "" && !agent.ValidModel(t.Model) {
		return w.reject(req.AttemptID, "bad_request", fmt.Sprintf("model %q is not a model id", t.Model))
	}
	b, err := agent.Get(t.Backend)
	if err != nil {
		return w.reject(req.AttemptID, "unsupported", err.Error())
	}
	caps := agent.BackendCapabilities(b)
	if t.Mode == agent.ModeAsk || !slices.Contains(caps.Modes, t.Mode) {
		return w.reject(req.AttemptID, "unsupported", "backend cannot enforce mode "+string(t.Mode))
	}
	if t.Effort != "" && !slices.Contains(caps.Efforts, t.Effort) {
		return w.reject(req.AttemptID, "unsupported", "backend cannot apply effort "+t.Effort)
	}
	// Test presence, not a field's zero value: even max_turns:0 is an option
	// the caller explicitly set. taskRequest has no record-compatibility decoder.
	var fields struct {
		Task map[string]json.RawMessage `json:"task"`
	}
	_ = json.Unmarshal(first.data, &fields)
	for _, option := range []string{"model", "mode", "effort", "timeout", "max_turns", "mcp"} {
		if _, set := fields.Task[option]; set && !slices.Contains(caps.Options, option) {
			return w.reject(req.AttemptID, "unsupported", "backend cannot honour option "+option)
		}
	}
	if t.Timeout < 0 || t.MaxTurns < 0 {
		return w.reject(req.AttemptID, "bad_request", "timeout and max_turns must not be negative")
	}
	if t.Purpose != "" && !purpose.Valid(t.Purpose) {
		return w.reject(req.AttemptID, "bad_request", fmt.Sprintf("purpose %q: one of %s", t.Purpose, strings.Join(purpose.Declarable(), ", ")))
	}
	if t.Purpose == "" {
		t.Purpose = purpose.Declared(t.Prompt)
	}
	ws, err := tools.NewWorkspace(t.Dir)
	if err != nil {
		return w.reject(req.AttemptID, "bad_request", err.Error())
	}
	task := agent.Task{Prompt: t.Prompt, Purpose: t.Purpose, Dir: ws.Root, AttemptID: req.AttemptID, Options: opts}
	// A backend without MCP support must not inherit the default config.
	if slices.Contains(caps.Options, "mcp") {
		if task.MCP, err = mcp.ConfigPaths(t.MCP); err != nil {
			return w.reject(req.AttemptID, "bad_request", err.Error())
		}
	}
	var plan followup.Plan
	if t.ContinueRunID != "" {
		plan, err = followup.Make(followup.Request{RunID: t.ContinueRunID, Backend: t.Backend, Dir: task.Dir, Model: t.Model, Fresh: t.Fresh})
		if err != nil {
			return w.reject(req.AttemptID, "bad_request", err.Error())
		}
		defer plan.Release()
		task.Dir, task.Model = plan.Dir, plan.Model
		if task.Model != "" && !agent.ValidModel(task.Model) {
			return w.reject(req.AttemptID, "bad_request", fmt.Sprintf("model %q from run %s is not a model id", task.Model, t.ContinueRunID))
		}
	}
	if _, err := b.Detect(); err != nil {
		return w.reject(req.AttemptID, "unavailable", err.Error())
	}
	rec, err := create(t.Backend, task, t.ContinueRunID, plan.Route(), routed, routeNote)
	if err != nil {
		return w.reject(req.AttemptID, "storage", err.Error())
	}
	task.RunDir = rec.Dir()
	w.attempt, w.runID = req.AttemptID, rec.ID()
	notes := append([]string{}, plan.Notes...)
	if warning := plan.ContextWarning(); warning != "" {
		notes = append(notes, warning)
	}
	if !w.write("accepted", map[string]any{
		"requested": map[string]any{"backend": t.Backend, "model": task.Model, "mode": t.Mode, "effort": t.Effort},
		"continues": t.ContinueRunID, "notes": notes,
	}) {
		_ = rec.Close(agent.Result{Backend: t.Backend, Status: agent.StatusCanceled, Error: "stdout failed before backend started"})
		return 1
	}

	var eventMu sync.Mutex
	storageFailed := false
	emit := func(e agent.Event) {
		if e.Partial {
			return
		}
		eventMu.Lock()
		defer eventMu.Unlock()
		if e.Time.IsZero() {
			e.Time = now()
		}
		rec.Event(e)
		if err := rec.Err(); err != nil && !storageFailed {
			storageFailed = true
			w.failure("storage", err.Error(), true)
			cancel()
		}
		truncated := len(e.Text) > MaxEventText
		if truncated {
			end := MaxEventText
			for end > 0 && !utf8.RuneStart(e.Text[end]) {
				end--
			}
			e.Text = e.Text[:end]
		}
		if len(e.Input) > MaxEventText {
			e.Input = nil
			truncated = true
		}
		w.write("event", map[string]any{"event": struct {
			agent.Event
			Truncated bool `json:"truncated"`
		}{e, truncated}})
	}
	result := make(chan agent.Result, 1)
	go func() {
		plan.Apply(&task, rec.Dir(), emit)
		result <- agent.Run(ctx, b, task, emit)
	}()
	interrupt := ctx.Done()
	for {
		select {
		case res := <-result:
			closeErr := rec.Close(res)
			recordError := ""
			if closeErr != nil {
				recordError = closeErr.Error()
				if !storageFailed {
					w.failure("storage", recordError, true)
				}
			}
			resolvedModel := ""
			if res.ModelReported {
				resolvedModel = res.Model
			}
			if !w.write("result", map[string]any{
				"result": res, "requested_model": task.Model, "resolved_model": resolvedModel, "record_error": recordError,
			}) {
				return 1
			}
			return 0
		case line := <-input:
			if line.err != nil {
				if errors.Is(line.err, errInputTooLong) {
					w.failure("bad_request", line.err.Error(), false)
					continue
				}
				if !errors.Is(line.err, io.EOF) {
					w.failure("bad_request", line.err.Error(), false)
				}
				cancel()
				input = nil
				continue
			}
			var c cancelRequest
			if err := decode(line.data, &c); err != nil {
				w.failure("bad_request", err.Error(), false)
				continue
			}
			if c.Type != "cancel" {
				w.failure("bad_request", "only cancel is accepted after start", false)
				continue
			}
			if c.AttemptID != req.AttemptID {
				w.failure("bad_request", "cancel names another attempt_id", false)
				continue
			}
			cancel()
		case <-interrupt:
			cancel()
			interrupt = nil
		}
	}
}

var attemptID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

type startRequest struct {
	Type      string       `json:"type"`
	AttemptID string       `json:"attempt_id"`
	Task      *taskRequest `json:"task"`
}

type taskRequest struct {
	Prompt        string         `json:"prompt"`
	Dir           string         `json:"dir"`
	Backend       string         `json:"backend"`
	Mode          agent.Mode     `json:"mode"`
	Model         string         `json:"model"`
	Effort        string         `json:"effort"`
	Timeout       agent.Duration `json:"timeout"`
	MaxTurns      int            `json:"max_turns"`
	MCP           []string       `json:"mcp"`
	ContinueRunID string         `json:"continue_run_id"`
	Purpose       string         `json:"purpose"`
	Fresh         bool           `json:"fresh"`
}

type cancelRequest struct {
	Type      string `json:"type"`
	AttemptID string `json:"attempt_id"`
}

func decode(data []byte, value any) error {
	if !utf8.Valid(data) {
		return errors.New("input must be UTF-8")
	}
	// encoding/json matches struct fields without regard to case and merges
	// repeated objects. Validate names and uniqueness before decoding so the
	// presence-based option gate sees exactly the fields that reach the task.
	fields, err := objectFields(data, "type", "attempt_id", "task")
	if err != nil {
		return err
	}
	var kind string
	// Classify without reporting value errors until all keys have been checked.
	// The typed decoder below still rejects an invalid type value.
	_ = json.Unmarshal(fields["type"], &kind)
	_, isCancel := value.(*cancelRequest)
	if kind == "cancel" || (isCancel && kind != "start") {
		if _, set := fields["task"]; set {
			return fmt.Errorf("unknown field %q", "task")
		}
	} else {
		if err := checkTaskKeys(fields["task"]); err != nil {
			return err
		}
	}
	if isCancel && kind == "start" {
		return errors.New("only cancel is accepted after start")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("each line must contain one JSON object")
	}
	return nil
}

func checkTaskKeys(data json.RawMessage) error {
	if len(data) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}
	_, err := objectFields(data, "prompt", "dir", "backend", "mode", "model", "effort", "timeout", "max_turns", "mcp", "continue_run_id", "purpose", "fresh")
	return err
}

func objectFields(data []byte, names ...string) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	first, err := d.Token()
	if err != nil {
		return nil, err
	}
	if first != json.Delim('{') {
		return nil, errors.New("each line must contain one JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return nil, err
		}
		key := token.(string)
		if !slices.Contains(names, key) {
			return nil, fmt.Errorf("unknown field %q", key)
		}
		if _, set := fields[key]; set {
			return nil, fmt.Errorf("duplicate field %q", key)
		}
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			return nil, err
		}
		fields[key] = raw
	}
	if _, err := d.Token(); err != nil {
		return nil, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, errors.New("each line must contain one JSON object")
	}
	return fields, nil
}

type inputLine struct {
	data []byte
	err  error
}

func readInput(in io.Reader, done <-chan struct{}) <-chan inputLine {
	ch := make(chan inputLine)
	go func() {
		r := bufio.NewReader(in)
		first := true
		for {
			data, err := readLine(r, !first)
			select {
			case ch <- inputLine{data, err}:
			case <-done:
				return
			}
			if err != nil && (first || !errors.Is(err, errInputTooLong)) {
				return
			}
			first = false
		}
	}()
	return ch
}

var errInputTooLong = errors.New("input line exceeds 4 MiB")

// readLine bounds allocation even when a peer never sends a newline. The
// limit counts the JSON bytes, excluding the framing newline. Later frames
// discard the entire oversized line, allowing the next control message through.
func readLine(r *bufio.Reader, discardOverLimit bool) ([]byte, error) {
	var line []byte
	overLimit := false
	for {
		part, err := r.ReadSlice('\n')
		if len(part) > 0 && part[len(part)-1] == '\n' {
			part = part[:len(part)-1]
		}
		if len(line)+len(part) > MaxInputLine {
			if !discardOverLimit {
				return nil, errInputTooLong
			}
			overLimit = true
			line = nil
		}
		if !overLimit {
			line = append(line, part...)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if overLimit {
			if err != nil {
				return nil, err
			}
			return nil, errInputTooLong
		}
		if err == io.EOF && len(line) != 0 {
			return nil, errors.New("input line must end with a newline")
		}
		return line, err
	}
}

type writer struct {
	mu             sync.Mutex
	out            io.Writer
	now            func() time.Time
	cancel         context.CancelFunc
	seq            uint64
	err            error
	attempt, runID string
}

func (w *writer) write(kind string, fields map[string]any) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return false
	}
	w.seq++
	fields["v"], fields["seq"], fields["type"], fields["time"] = 1, w.seq, kind, w.now().Format(time.RFC3339Nano)
	if w.attempt != "" {
		fields["attempt_id"] = w.attempt
	}
	if w.runID != "" {
		fields["run_id"] = w.runID
	}
	data, err := json.Marshal(fields)
	if err == nil {
		data = append(data, '\n')
		var n int
		n, err = w.out.Write(data)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
	}
	if err != nil {
		w.err = err
		w.cancel()
		return false
	}
	return true
}

func (w *writer) reject(attempt, code, message string) int {
	w.attempt = attempt
	if !w.write("rejected", map[string]any{"error": protocolError{code, message}}) {
		return 1
	}
	return 2
}

func (w *writer) failure(code, message string, fatal bool) {
	w.write("error", map[string]any{"error": protocolError{code, message}, "fatal": fatal})
}

type protocolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func hello() map[string]any {
	backends := make([]any, 0)
	for _, name := range agent.Names() {
		b, _ := agent.Get(name)
		detail, err := b.Detect()
		if err != nil {
			detail = err.Error()
		}
		backends = append(backends, struct {
			Name      string `json:"name"`
			Available bool   `json:"available"`
			Detail    string `json:"detail"`
			agent.Capabilities
		}{name, err == nil, detail, agent.BackendCapabilities(b)})
	}
	revision := "unknown"
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && setting.Value != "" {
				revision = setting.Value
			}
		}
	}
	return map[string]any{
		"protocol": "v1", "pid": os.Getpid(), "backends": backends,
		"hrn":    map[string]string{"version": agent.Version, "revision": revision, "go": runtime.Version()},
		"limits": map[string]int{"max_input_line": MaxInputLine, "max_event_text": MaxEventText},
	}
}
