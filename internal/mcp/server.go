package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"strconv"
	"sync"
)

// Tool is a tool a Server offers.
type Tool struct {
	Name        string
	Title       string
	Description string
	// InputSchema defaults to an object with no properties.
	InputSchema map[string]any
	Annotations *Annotations
	Handler     func(ctx context.Context, call Call) (CallResult, error)
}

// Call enables progress only when the client requests it.
type Call struct {
	Name      string
	Arguments json.RawMessage
	Progress  func(message string)

	conn *conn
}

// ClientSupports checks declared handshake capabilities.
func (c Call) ClientSupports(capability string) bool {
	if c.conn == nil {
		return false
	}
	return c.conn.supports(capability)
}

// Request asks the client a method and decodes its answer into out.
// Set a deadline: unanswered requests otherwise wait for cancellation.
func (c Call) Request(ctx context.Context, method string, params, out any) error {
	if c.conn == nil {
		return errors.New("this call has no connection to ask on")
	}
	return c.conn.request(ctx, method, params, out)
}

type conn struct {
	codec *codec

	mu      sync.Mutex
	caps    map[string]any
	nextID  int64
	pending map[string]chan *message
}

func newConn(c *codec) *conn {
	return &conn{codec: c, pending: map[string]chan *message{}}
}

func (cn *conn) setCaps(caps map[string]any) {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	cn.caps = caps
}

func (cn *conn) supports(capability string) bool {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	_, ok := cn.caps[capability]
	return ok
}

func (cn *conn) route(m *message) bool {
	cn.mu.Lock()
	ch := cn.pending[string(m.ID)]
	delete(cn.pending, string(m.ID))
	cn.mu.Unlock()
	if ch == nil {
		return false
	}
	ch <- m
	return true
}

func (cn *conn) request(ctx context.Context, method string, params, out any) error {
	cn.mu.Lock()
	cn.nextID++
	id := json.RawMessage(strconv.FormatInt(cn.nextID, 10))
	ch := make(chan *message, 1)
	cn.pending[string(id)] = ch
	cn.mu.Unlock()

	if err := cn.codec.request(id, method, params); err != nil {
		cn.mu.Lock()
		delete(cn.pending, string(id))
		cn.mu.Unlock()
		return err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return m.Error
		}
		if out != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, out)
		}
		return nil
	case <-ctx.Done():
		cn.mu.Lock()
		delete(cn.pending, string(id))
		cn.mu.Unlock()
		_ = cn.codec.notify("notifications/cancelled", cancelledParams{RequestID: id, Reason: ctx.Err().Error()})
		return ctx.Err()
	}
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

// Serve runs requests concurrently until EOF or cancellation, canceling handlers on shutdown.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	c := newCodec(r, w)
	cn := newConn(c)
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
	// Cancel before waiting so unanswered client requests cannot stall shutdown.
	defer func() { cancel(); wg.Wait() }()

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
					s.handle(reqCtx, cn, m)
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
				if m.isResponse() {
					cn.route(m)
				}
			}
		}
	}
}

func (s *Server) handle(ctx context.Context, cn *conn, m *message) {
	c := cn.codec
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
		cn.setCaps(p.Capabilities)
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
		call := Call{Name: p.Name, Arguments: p.Arguments, Progress: func(string) {}, conn: cn}
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
