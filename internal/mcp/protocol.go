// Package mcp is a dependency-free implementation of the Model Context
// Protocol over its stdio transport: JSON-RPC 2.0 messages, one per line.
//
// Client lets the api backend call tools served by external MCP servers.
// Server exposes hrn itself to MCP hosts such as Claude Code. Only the tools
// capability is implemented, plus ping, progress, cancellation and roots;
// resources, prompts and sampling are not.
package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
)

// ProtocolVersion is the spec revision hrn asks for. Everything hrn uses has
// been wire-stable since the first revision, so older peers negotiate fine.
const ProtocolVersion = "2025-06-18"

var supportedVersions = []string{ProtocolVersion, "2025-03-26", "2024-11-05"}

func versionSupported(v string) bool {
	for _, s := range supportedVersions {
		if s == v {
			return true
		}
	}
	return false
}

// JSON-RPC 2.0 error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Error is a JSON-RPC error. Tool execution failures are not Errors: the
// protocol reports those inside CallResult.IsError so the model can react.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s (jsonrpc %d)", e.Message, e.Code) }

// message is the union of request, notification and response. Which one it
// is follows from which fields are set.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

func (m *message) hasID() bool          { return len(m.ID) > 0 && !bytes.Equal(m.ID, []byte("null")) }
func (m *message) isRequest() bool      { return m.Method != "" && m.hasID() }
func (m *message) isNotification() bool { return m.Method != "" && !m.hasID() }
func (m *message) isResponse() bool     { return m.Method == "" && m.hasID() }

// codec frames messages as newline-delimited JSON. Writes are serialized so
// concurrent handlers never interleave bytes on the pipe.
type codec struct {
	r  *bufio.Reader
	mu sync.Mutex
	w  io.Writer
}

func newCodec(r io.Reader, w io.Writer) *codec {
	return &codec{r: bufio.NewReaderSize(r, 64*1024), w: w}
}

// read returns the next message. Malformed JSON comes back as an *Error with
// CodeParseError so the caller can answer it and keep going; anything else
// is an I/O error and ends the connection.
func (c *codec) read() (*message, error) {
	for {
		line, err := c.r.ReadBytes('\n')
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			if err != nil {
				return nil, err
			}
			continue
		}
		var m message
		if jerr := json.Unmarshal(line, &m); jerr != nil {
			return nil, &Error{Code: CodeParseError, Message: jerr.Error()}
		}
		// A final line without a newline still parsed; EOF surfaces next read.
		return &m, nil
	}
}

func (c *codec) write(m *message) error {
	m.JSONRPC = "2.0"
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.w.Write(append(data, '\n'))
	return err
}

func (c *codec) request(id json.RawMessage, method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	return c.write(&message{ID: id, Method: method, Params: raw})
}

func (c *codec) notify(method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	return c.write(&message{Method: method, Params: raw})
}

func (c *codec) respond(id json.RawMessage, result any) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return c.fail(id, CodeInternalError, err.Error())
	}
	return c.write(&message{ID: id, Result: raw})
}

func (c *codec) fail(id json.RawMessage, code int, msg string) error {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return c.write(&message{ID: id, Error: &Error{Code: code, Message: msg}})
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	if raw, ok := params.(json.RawMessage); ok {
		return raw, nil
	}
	return json.Marshal(params)
}

// ---- protocol types ---------------------------------------------------------

type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      Implementation `json:"clientInfo"`
}

type initializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      Implementation `json:"serverInfo"`
	Instructions    string         `json:"instructions,omitempty"`
}

// Annotations are a server's hints about how a tool behaves. They are only as
// trustworthy as the server that sent them, which the user chose to configure.
type Annotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// ReadOnly reports whether the server marked the tool as having no side effects.
func (a *Annotations) ReadOnly() bool { return a != nil && a.ReadOnlyHint != nil && *a.ReadOnlyHint }

func Bool(v bool) *bool { return &v }

// ToolInfo is a tool as a server lists it.
type ToolInfo struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations *Annotations    `json:"annotations,omitempty"`
}

type listToolsParams struct {
	Cursor string `json:"cursor,omitempty"`
}

type listToolsResult struct {
	Tools      []ToolInfo `json:"tools"`
	NextCursor string     `json:"nextCursor,omitempty"`
}

type callMeta struct {
	ProgressToken json.RawMessage `json:"progressToken,omitempty"`
}

type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Meta      *callMeta       `json:"_meta,omitempty"`
}

// Content is one part of a tool result: text, image, audio, resource_link or
// an embedded resource.
type Content struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	Data     string          `json:"data,omitempty"`
	MimeType string          `json:"mimeType,omitempty"`
	URI      string          `json:"uri,omitempty"`
	Name     string          `json:"name,omitempty"`
	Resource json.RawMessage `json:"resource,omitempty"`
}

func TextContent(s string) Content { return Content{Type: "text", Text: s} }

type CallResult struct {
	Content           []Content `json:"content"`
	StructuredContent any       `json:"structuredContent,omitempty"`
	IsError           bool      `json:"isError,omitempty"`
}

// Text flattens a result for a model: text parts verbatim, binary parts as a
// one-line placeholder, and structured content as JSON when nothing else is
// there.
func (r CallResult) Text() string {
	var b strings.Builder
	for i, c := range r.Content {
		if i > 0 {
			b.WriteString("\n")
		}
		switch c.Type {
		case "text":
			b.WriteString(c.Text)
		case "image", "audio":
			fmt.Fprintf(&b, "[%s %s, %d bytes]", c.Type, c.MimeType, len(c.Data)*3/4)
		case "resource_link":
			fmt.Fprintf(&b, "[resource %s]", c.URI)
		case "resource":
			var res struct {
				URI  string `json:"uri"`
				Text string `json:"text"`
			}
			if json.Unmarshal(c.Resource, &res) == nil && res.Text != "" {
				b.WriteString(res.Text)
			} else {
				fmt.Fprintf(&b, "[resource %s]", res.URI)
			}
		default:
			fmt.Fprintf(&b, "[%s]", c.Type)
		}
	}
	if b.Len() == 0 && r.StructuredContent != nil {
		data, _ := json.Marshal(r.StructuredContent)
		b.Write(data)
	}
	return b.String()
}

type progressParams struct {
	ProgressToken json.RawMessage `json:"progressToken"`
	Progress      float64         `json:"progress"`
	Total         *float64        `json:"total,omitempty"`
	Message       string          `json:"message,omitempty"`
}

type cancelledParams struct {
	RequestID json.RawMessage `json:"requestId"`
	Reason    string          `json:"reason,omitempty"`
}

type logParams struct {
	Level  string          `json:"level"`
	Logger string          `json:"logger,omitempty"`
	Data   json.RawMessage `json:"data"`
}

type Root struct {
	URI  string `json:"uri"`
	Name string `json:"name,omitempty"`
}
