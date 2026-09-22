package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ServerConfig describes how to start a stdio MCP server.
type ServerConfig struct {
	Name    string            `json:"-"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// Dir is the server's working directory; hx uses the task workspace.
	Dir string `json:"-"`
}

// Client talks to one MCP server. Zero value fields are optional; call Start
// (spawn a process) or Connect (existing pipes) before anything else.
type Client struct {
	// Info is sent as clientInfo. Defaults to hx.
	Info Implementation
	// Roots are the directories reported to servers that ask (roots/list).
	Roots []string
	// Capabilities are declared to the server on top of roots, so it knows what
	// it may ask this client to do.
	Capabilities map[string]any
	// Elicit answers a server's elicitation/create. Nil means the client cannot
	// ask its user anything, and the server is told so.
	Elicit func(ctx context.Context, params json.RawMessage) (any, error)
	// Log receives the server's warning/error log notifications. Nil discards.
	Log func(string)
	// Progress receives progress messages for in-flight tool calls. When set,
	// calls ask the server for progress; when nil they do not.
	Progress func(string)

	// StartupTimeout bounds the initialize handshake. Default 60s: npx servers
	// may install on first use.
	StartupTimeout time.Duration

	cmd    *exec.Cmd
	stdin  io.Closer
	stderr *tail
	codec  *codec

	mu      sync.Mutex
	nextID  int64
	pending map[string]chan *message
	done    chan struct{} // closed when the read loop exits
	exitErr error

	server       Implementation
	instructions string
	tools        []ToolInfo
}

// Start spawns the server process and performs the handshake.
func (c *Client) Start(ctx context.Context, cfg ServerConfig) error {
	if cfg.Command == "" {
		return errors.New("mcp: empty command")
	}
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Dir = cfg.Dir
	cmd.Env = os.Environ()
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	c.stderr = &tail{max: 20}
	cmd.Stderr = c.stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("mcp: starting %s: %w", cfg.Command, err)
	}
	c.cmd, c.stdin = cmd, stdin
	if err := c.Connect(ctx, stdout, stdin); err != nil {
		_ = c.Close()
		if lines := c.stderr.String(); lines != "" {
			err = fmt.Errorf("%w\n%s", err, lines)
		}
		return err
	}
	return nil
}

// Connect performs the handshake over an existing pair of streams (r carries
// the server's output, w feeds its input) and lists the server's tools.
func (c *Client) Connect(ctx context.Context, r io.Reader, w io.Writer) error {
	if c.Info.Name == "" {
		c.Info = Implementation{Name: "hx", Version: "dev"}
	}
	if c.StartupTimeout <= 0 {
		c.StartupTimeout = 60 * time.Second
	}
	c.codec = newCodec(r, w)
	c.pending = map[string]chan *message{}
	c.done = make(chan struct{})
	go c.readLoop()

	hctx, cancel := context.WithTimeout(ctx, c.StartupTimeout)
	defer cancel()
	var init initializeResult
	caps := map[string]any{"roots": map[string]any{"listChanged": false}}
	for k, v := range c.Capabilities {
		caps[k] = v
	}
	if c.Elicit != nil {
		caps["elicitation"] = map[string]any{}
	}
	err := c.call(hctx, "initialize", initializeParams{
		ProtocolVersion: ProtocolVersion,
		Capabilities:    caps,
		ClientInfo:      c.Info,
	}, &init)
	if err != nil {
		return fmt.Errorf("mcp: initialize: %w", err)
	}
	c.server, c.instructions = init.ServerInfo, init.Instructions
	if err := c.codec.notify("notifications/initialized", nil); err != nil {
		return err
	}
	tools, err := c.listTools(hctx)
	if err != nil {
		return fmt.Errorf("mcp: tools/list: %w", err)
	}
	c.tools = tools
	return nil
}

func (c *Client) listTools(ctx context.Context) ([]ToolInfo, error) {
	var all []ToolInfo
	cursor := ""
	for {
		var page listToolsResult
		if err := c.call(ctx, "tools/list", listToolsParams{Cursor: cursor}, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Tools...)
		if page.NextCursor == "" || page.NextCursor == cursor {
			return all, nil
		}
		cursor = page.NextCursor
	}
}

// ServerInfo, Instructions and Tools report what the handshake returned.
func (c *Client) ServerInfo() Implementation { return c.server }
func (c *Client) Instructions() string       { return c.instructions }
func (c *Client) Tools() []ToolInfo          { return c.tools }

// CallTool invokes a tool. A tool-reported failure comes back as
// CallResult.IsError with a nil error; a nil error and IsError false is success.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (CallResult, error) {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	params := callParams{Name: name, Arguments: args}
	if c.Progress != nil {
		params.Meta = &callMeta{ProgressToken: json.RawMessage(strconv.FormatInt(time.Now().UnixNano(), 10))}
	}
	var res CallResult
	err := c.call(ctx, "tools/call", params, &res)
	return res, err
}

// Ping checks the server is alive.
func (c *Client) Ping(ctx context.Context) error {
	return c.call(ctx, "ping", nil, nil)
}

// Close ends the session. A spawned server gets its stdin closed first and is
// killed if it does not exit promptly.
func (c *Client) Close() error {
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.cmd == nil {
		return nil
	}
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
		_ = c.cmd.Process.Kill()
	}
	err := c.cmd.Wait()
	<-c.done
	c.cmd = nil
	return err
}

func (c *Client) call(ctx context.Context, method string, params any, result any) error {
	c.mu.Lock()
	if c.exitErr != nil {
		c.mu.Unlock()
		return c.exitErr
	}
	c.nextID++
	id := json.RawMessage(strconv.FormatInt(c.nextID, 10))
	ch := make(chan *message, 1)
	c.pending[string(id)] = ch
	c.mu.Unlock()

	if err := c.codec.request(id, method, params); err != nil {
		c.unregister(string(id))
		return err
	}
	select {
	case m := <-ch:
		if m == nil {
			return c.exitError()
		}
		if m.Error != nil {
			return m.Error
		}
		if result != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, result)
		}
		return nil
	case <-ctx.Done():
		c.unregister(string(id))
		_ = c.codec.notify("notifications/cancelled", cancelledParams{RequestID: id, Reason: ctx.Err().Error()})
		return ctx.Err()
	}
}

func (c *Client) unregister(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *Client) exitError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exitErr
}

func (c *Client) readLoop() {
	defer close(c.done)
	for {
		m, err := c.codec.read()
		if err != nil {
			var e *Error
			if errors.As(err, &e) && e.Code == CodeParseError {
				c.log("server sent invalid JSON: " + e.Message)
				continue
			}
			if errors.Is(err, io.EOF) {
				err = errors.New("mcp: server closed the connection")
			}
			c.mu.Lock()
			c.exitErr = err
			for id, ch := range c.pending {
				close(ch)
				delete(c.pending, id)
			}
			c.mu.Unlock()
			return
		}
		switch {
		case m.isResponse():
			c.mu.Lock()
			ch := c.pending[string(m.ID)]
			delete(c.pending, string(m.ID))
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		case m.isRequest():
			go c.handleRequest(m)
		case m.isNotification():
			c.handleNotification(m)
		}
	}
}

// handleRequest answers the few requests a server may send to a client.
func (c *Client) handleRequest(m *message) {
	switch m.Method {
	case "ping":
		_ = c.codec.respond(m.ID, struct{}{})
	case "elicitation/create":
		if c.Elicit == nil {
			_ = c.codec.fail(m.ID, CodeMethodNotFound, "this client cannot ask its user")
			return
		}
		answer, err := c.Elicit(context.Background(), m.Params)
		if err != nil {
			_ = c.codec.fail(m.ID, CodeInternalError, err.Error())
			return
		}
		_ = c.codec.respond(m.ID, answer)
	case "roots/list":
		roots := make([]Root, 0, len(c.Roots))
		for _, r := range c.Roots {
			roots = append(roots, Root{URI: fileURI(r), Name: filepath.Base(r)})
		}
		_ = c.codec.respond(m.ID, map[string]any{"roots": roots})
	default:
		_ = c.codec.fail(m.ID, CodeMethodNotFound, "client does not support "+m.Method)
	}
}

func (c *Client) handleNotification(m *message) {
	switch m.Method {
	case "notifications/message":
		var p logParams
		if json.Unmarshal(m.Params, &p) != nil {
			return
		}
		switch p.Level {
		case "warning", "error", "critical", "alert", "emergency":
			var s string
			if json.Unmarshal(p.Data, &s) != nil {
				s = string(p.Data)
			}
			c.log(p.Level + ": " + s)
		}
	case "notifications/progress":
		if c.Progress == nil {
			return
		}
		var p progressParams
		if json.Unmarshal(m.Params, &p) == nil && p.Message != "" {
			c.Progress(p.Message)
		}
	}
}

func (c *Client) log(s string) {
	if c.Log != nil {
		c.Log(s)
	}
}

func fileURI(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // Windows drive paths: file:///C:/dir
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

// tail keeps the last lines written to it, for error messages.
type tail struct {
	mu    sync.Mutex
	max   int
	lines []string
	part  string
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.part += string(p)
	for {
		i := strings.IndexByte(t.part, '\n')
		if i < 0 {
			break
		}
		t.push(t.part[:i])
		t.part = t.part[i+1:]
	}
	return len(p), nil
}

func (t *tail) push(line string) {
	line = strings.TrimRight(line, "\r")
	if strings.TrimSpace(line) == "" {
		return
	}
	t.lines = append(t.lines, line)
	if len(t.lines) > t.max {
		t.lines = t.lines[1:]
	}
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := t.lines
	if strings.TrimSpace(t.part) != "" {
		lines = append(append([]string(nil), lines...), t.part)
	}
	return strings.Join(lines, "\n")
}
