package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"sync"
)

// Tool is a tool a Server offers.
type Tool struct {
	Name        string
	Title       string
	Description string
	// InputSchema is a JSON Schema object; nil means an object with no properties.
	InputSchema map[string]any
	Annotations *Annotations
	Handler     func(ctx context.Context, call Call) (CallResult, error)
}

// Call is one tools/call request as a handler sees it. Progress sends a
// progress notification when the client asked for them and is a no-op
// otherwise. The context is cancelled if the client cancels the request.
type Call struct {
	Name      string
	Arguments json.RawMessage
	Progress  func(message string)
}

// Server serves tools to one client over a pair of streams.
type Server struct {
	Info         Implementation
	Instructions string

	tools  []Tool
	byName map[string]Tool
}

func NewServer(info Implementation, instructions string) *Server {
	return &Server{Info: info, Instructions: instructions, byName: map[string]Tool{}}
}

func (s *Server) AddTool(t Tool) {
	if _, dup := s.byName[t.Name]; dup {
		panic("mcp: duplicate tool " + t.Name)
	}
	s.tools = append(s.tools, t)
	s.byName[t.Name] = t
}

// Serve handles requests until the client closes r or ctx ends. Requests run
// concurrently, so a host can dispatch several long tool calls at once.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	c := newCodec(r, w)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type incoming struct {
		m   *message
		err error
	}
	in := make(chan incoming)
	go func() {
		for {
			m, err := c.read()
			select {
			case in <- incoming{m, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				var e *Error
				if !errors.As(err, &e) {
					return
				}
			}
		}
	}()

	var wg sync.WaitGroup
	var mu sync.Mutex
	inflight := map[string]context.CancelFunc{}
	defer wg.Wait()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg := <-in:
			if msg.err != nil {
				var e *Error
				if errors.As(msg.err, &e) && e.Code == CodeParseError {
					_ = c.fail(nil, CodeParseError, e.Message)
					continue
				}
				if errors.Is(msg.err, io.EOF) {
					return nil
				}
				return msg.err
			}
			m := msg.m
			switch {
			case m.isRequest():
				reqCtx, reqCancel := context.WithCancel(ctx)
				mu.Lock()
				inflight[string(m.ID)] = reqCancel
				mu.Unlock()
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() {
						mu.Lock()
						delete(inflight, string(m.ID))
						mu.Unlock()
						reqCancel()
					}()
					s.handle(reqCtx, c, m)
				}()
			case m.isNotification():
				if m.Method == "notifications/cancelled" {
					var p cancelledParams
					if json.Unmarshal(m.Params, &p) == nil {
						mu.Lock()
						if cancelReq := inflight[string(p.RequestID)]; cancelReq != nil {
							cancelReq()
						}
						mu.Unlock()
					}
				}
			default:
				// Responses to requests we never sent: ignore.
			}
		}
	}
}

func (s *Server) handle(ctx context.Context, c *codec, m *message) {
	defer func() {
		if r := recover(); r != nil {
			_ = c.fail(m.ID, CodeInternalError, fmt.Sprintf("%s: panic: %v\n%s", m.Method, r, debug.Stack()))
		}
	}()
	switch m.Method {
	case "initialize":
		var p initializeParams
		if err := json.Unmarshal(m.Params, &p); err != nil {
			_ = c.fail(m.ID, CodeInvalidParams, err.Error())
			return
		}
		version := ProtocolVersion
		if versionSupported(p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		_ = c.respond(m.ID, initializeResult{
			ProtocolVersion: version,
			Capabilities:    map[string]any{"tools": map[string]any{"listChanged": false}},
			ServerInfo:      s.Info,
			Instructions:    s.Instructions,
		})
	case "ping":
		_ = c.respond(m.ID, struct{}{})
	case "tools/list":
		infos := make([]ToolInfo, 0, len(s.tools))
		for _, t := range s.tools {
			schema := t.InputSchema
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			raw, _ := json.Marshal(schema)
			infos = append(infos, ToolInfo{Name: t.Name, Title: t.Title, Description: t.Description, InputSchema: raw, Annotations: t.Annotations})
		}
		_ = c.respond(m.ID, listToolsResult{Tools: infos})
	case "tools/call":
		var p callParams
		if err := json.Unmarshal(m.Params, &p); err != nil {
			_ = c.fail(m.ID, CodeInvalidParams, err.Error())
			return
		}
		t, ok := s.byName[p.Name]
		if !ok {
			_ = c.fail(m.ID, CodeInvalidParams, "unknown tool "+p.Name)
			return
		}
		call := Call{Name: p.Name, Arguments: p.Arguments, Progress: func(string) {}}
		if len(call.Arguments) == 0 {
			call.Arguments = json.RawMessage("{}")
		}
		if p.Meta != nil && len(p.Meta.ProgressToken) > 0 {
			var n float64
			var pmu sync.Mutex
			call.Progress = func(msg string) {
				pmu.Lock()
				n++
				_ = c.notify("notifications/progress", progressParams{ProgressToken: p.Meta.ProgressToken, Progress: n, Message: msg})
				pmu.Unlock()
			}
		}
		res, err := t.Handler(ctx, call)
		if err != nil {
			res = CallResult{Content: []Content{TextContent(err.Error())}, IsError: true}
		}
		if res.Content == nil {
			res.Content = []Content{}
		}
		_ = c.respond(m.ID, res)
	default:
		_ = c.fail(m.ID, CodeMethodNotFound, "unknown method "+m.Method)
	}
}
