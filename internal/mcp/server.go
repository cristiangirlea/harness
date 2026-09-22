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

	conn *conn
}

// ClientSupports reports whether the client declared a capability, such as
// "elicitation", when it connected.
func (c Call) ClientSupports(capability string) bool {
	if c.conn == nil {
		return false
	}
	return c.conn.supports(capability)
}

// Request asks the client something and waits for its answer, which is
// decoded into out. It is how a tool reaches the person at the other end:
// elicitation/create is the only method MCP defines for it today.
//
// The wait ends only with ctx or an answer, and a person is on the other side,
// so give ctx a deadline. Without one the call waits until the client answers
// or the connection drops.
func (c Call) Request(ctx context.Context, method string, params, out any) error {
	if c.conn == nil {
		return errors.New("this call has no connection to ask on")
	}
	return c.conn.request(ctx, method, params, out)
}

// conn is one client connection: the framing, what the client can do, and the
// requests this server is waiting on answers for.
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

// route hands a response to whoever is waiting for it. It reports whether
// anyone was.
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

// Serve handles requests until the client closes r or ctx ends. Requests run
// concurrently, so a host can dispatch several long tool calls at once.
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
	// Cancel first, then wait: a handler blocked on an answer from a client that
	// has gone away is released by the cancel, and waiting first would hold the
	// shutdown until its own timeout.
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
				// An answer to something this server asked the client.
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
