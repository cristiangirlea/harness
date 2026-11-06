package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func callResultMessage(t *testing.T, s *Server) *message {
	t.Helper()
	var out bytes.Buffer
	s.handle(context.Background(), newConn(newCodec(strings.NewReader(""), &out)), &message{
		ID: json.RawMessage("1"), Method: "tools/call", Params: json.RawMessage(`{"name":"test"}`),
	})
	var m message
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return &m
}

func TestToolPanicLogsStack(t *testing.T) {
	s := NewServer(Implementation{}, "")
	var log bytes.Buffer
	s.Log = &log
	s.AddTool(Tool{Name: "test", Handler: func(context.Context, Call) (CallResult, error) {
		panic("private panic value")
	}})
	m := callResultMessage(t, s)
	if m.Error == nil || m.Error.Code != CodeInternalError || m.Error.Message != "tools/call: internal error" {
		t.Errorf("panic response = %+v", m.Error)
	}
	if strings.Contains(string(m.Result), "goroutine") || (m.Error != nil && strings.Contains(m.Error.Message, "goroutine")) {
		t.Error("panic response leaked a stack")
	}
	if !strings.Contains(log.String(), "private panic value") || !strings.Contains(log.String(), "goroutine") || !strings.Contains(log.String(), "runtime/debug.Stack") {
		t.Errorf("panic log missing value or stack: %q", log.String())
	}
}

func TestToolResultLimit(t *testing.T) {
	for _, hint := range []string{"", "Request fewer events."} {
		for _, delta := range []int{-1, 0, 1} {
			t.Run(fmt.Sprintf("%s/%d", hint, delta), func(t *testing.T) {
				res := CallResult{Content: []Content{TextContent("unchanged")}, StructuredContent: map[string]any{"data": ""}}
				empty, err := json.Marshal(res)
				if err != nil {
					t.Fatal(err)
				}
				res.StructuredContent = map[string]any{"data": strings.Repeat("x", MaxResultBytes-len(empty)+delta)}
				raw, err := json.Marshal(res)
				if err != nil {
					t.Fatal(err)
				}
				s := NewServer(Implementation{}, "")
				s.AddTool(Tool{Name: "test", TooLarge: hint, Handler: func(context.Context, Call) (CallResult, error) { return res, nil }})
				m := callResultMessage(t, s)
				if m.Error != nil {
					t.Fatal(m.Error)
				}
				if delta <= 0 {
					if !bytes.Equal(m.Result, raw) {
						t.Fatal("result within the limit changed")
					}
					return
				}
				var got CallResult
				if err := json.Unmarshal(m.Result, &got); err != nil {
					t.Fatal(err)
				}
				wantHint := hint
				if wantHint == "" {
					wantHint = "Ask for less: narrow the arguments."
				}
				if !got.IsError || len(got.Content) != 1 || got.Content[0].Type != "text" ||
					!strings.HasPrefix(got.Text(), "result_too_large: ") || !strings.Contains(got.Text(), "131072-byte limit.") || !strings.HasSuffix(got.Text(), wantHint) {
					t.Fatalf("oversized result: isError=%v content count=%d", got.IsError, len(got.Content))
				}
				sc, ok := got.StructuredContent.(map[string]any)
				if !ok || len(sc) != 4 || sc["error"] != "result_too_large" || sc["bytes"] != float64(len(raw)) || sc["limit"] != float64(MaxResultBytes) || sc["hint"] != wantHint {
					t.Errorf("structured error = %v", got.StructuredContent)
				}
			})
		}
	}
}

func TestToolResultLimitKeepsIDs(t *testing.T) {
	final := strings.Repeat("x", 100<<10)
	object := map[string]any{
		"final": final, "run_id": "run-1", "limited_run_id": "limited-1", "batch_id": "batch-1",
		"long_id": strings.Repeat("x", 201), "number_id": 123, "nested": map[string]any{"other_id": "nested"},
		"id": "not-a-suffix", "boundary_id": strings.Repeat("é", 100), "multibyte_id": strings.Repeat("é", 101),
	}
	raw, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]any{"map": object, "json object": json.RawMessage(raw), "non-object": []string{"run-1"}} {
		t.Run(name, func(t *testing.T) {
			body := final
			if name == "non-object" {
				body += final
			}
			s := NewServer(Implementation{}, "")
			s.AddTool(Tool{Name: "test", Handler: func(context.Context, Call) (CallResult, error) {
				return CallResult{Content: []Content{TextContent(body)}, StructuredContent: content}, nil
			}})
			m := callResultMessage(t, s)
			if m.Error != nil {
				t.Fatal(m.Error)
			}
			var got CallResult
			if err := json.Unmarshal(m.Result, &got); err != nil {
				t.Fatal(err)
			}
			if !got.IsError || !strings.HasPrefix(got.Text(), "result_too_large:") {
				t.Fatal("missing size error")
			}
			sc, ok := got.StructuredContent.(map[string]any)
			if !ok {
				t.Fatal("missing structured error")
			}
			if name == "non-object" {
				if len(sc) != 4 {
					t.Fatalf("non-object copied fields: %v", sc)
				}
				return
			}
			for _, key := range []string{"batch_id", "boundary_id", "limited_run_id", "run_id"} {
				if sc[key] != object[key] {
					t.Errorf("lost %s", key)
				}
			}
			if len(sc) != 8 {
				t.Errorf("unexpected structured fields: %v", sc)
			}
			wantIDs := " batch_id=batch-1 boundary_id=" + object["boundary_id"].(string) + " limited_run_id=limited-1 run_id=run-1"
			if !strings.HasSuffix(got.Text(), wantIDs) {
				t.Errorf("missing or unordered IDs in %q", got.Text())
			}
		})
	}
}

type yieldingLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *yieldingLog) Write(p []byte) (int, error) {
	for _, line := range bytes.SplitAfter(p, []byte("\n")) {
		l.mu.Lock()
		_, _ = l.buf.Write(line)
		l.mu.Unlock()
		runtime.Gosched()
	}
	return len(p), nil
}

func TestToolConcurrentPanicsLogCompleteStacks(t *testing.T) {
	const calls = 8
	s := NewServer(Implementation{}, "")
	var log yieldingLog
	s.Log = &log
	var ready, done sync.WaitGroup
	ready.Add(calls)
	s.AddTool(Tool{Name: "test", Handler: func(context.Context, Call) (CallResult, error) {
		ready.Done()
		ready.Wait()
		panic("concurrent panic")
	}})
	for range calls {
		done.Go(func() {
			m := callResultMessage(t, s)
			if m.Error == nil || m.Error.Code != CodeInternalError || m.Error.Message != "tools/call: internal error" {
				t.Errorf("panic response = %+v", m.Error)
			}
		})
	}
	done.Wait()
	entries := strings.Split(log.buf.String(), "tools/call: panic: concurrent panic\n")
	if len(entries) != calls+1 || entries[0] != "" {
		t.Fatalf("got %d stack entries, want %d", len(entries)-1, calls)
	}
	for i, entry := range entries[1:] {
		if !strings.HasPrefix(entry, "goroutine ") || !strings.HasSuffix(entry, "\n") || strings.Count(entry, "runtime/debug.Stack()") != 1 ||
			!strings.Contains(entry, "created by sync.(*WaitGroup).Go") {
			t.Errorf("stack entry %d is incomplete or interleaved: %q", i, entry)
		}
	}
}

type invalidResultJSON struct{}

func (invalidResultJSON) MarshalJSON() ([]byte, error) {
	return nil, errors.New("cannot encode\nthis result\rnow")
}

func TestToolResultMarshalError(t *testing.T) {
	s := NewServer(Implementation{}, "")
	s.AddTool(Tool{Name: "test", Handler: func(context.Context, Call) (CallResult, error) {
		return CallResult{StructuredContent: invalidResultJSON{}}, nil
	}})
	m := callResultMessage(t, s)
	if m.Error != nil {
		t.Fatalf("marshal failure became an RPC error: %v", m.Error)
	}
	var got CallResult
	if err := json.Unmarshal(m.Result, &got); err != nil {
		t.Fatal(err)
	}
	if !got.IsError || len(got.Content) != 1 || got.Content[0].Type != "text" || !strings.Contains(got.Text(), "cannot encode") || strings.ContainsAny(got.Text(), "\r\n") {
		t.Errorf("marshal failure result = %+v", got)
	}
}
