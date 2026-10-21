// Package mcp implements MCP tools over stdio with one JSON-RPC 2.0 message per line.
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

// ProtocolVersion prefers this MCP revision while allowing wire-compatible older peers.
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

// CodeParseError and its peers are JSON-RPC 2.0 error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Error represents RPC failures; tool failures use IsError so the model can react.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s (jsonrpc %d)", e.Message, e.Code) }

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

// Serialize newline-framed JSON writes to prevent interleaved messages.
type codec struct {
	r  *bufio.Reader
	mu sync.Mutex
	w  io.Writer
}

func newCodec(r io.Reader, w io.Writer) *codec {
	return &codec{r: bufio.NewReaderSize(r, 64*1024), w: w}
}

// Parse errors allow a reply and continued reading; I/O errors end the connection.
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

// Annotations are hints only as trustworthy as their configured server.
type Annotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// ReadOnly reports the server's no-side-effects hint.
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

// Content holds text, image, audio, resource_link or resource result parts.
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

// Text flattens content with binary placeholders or falls back to structured JSON.
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
