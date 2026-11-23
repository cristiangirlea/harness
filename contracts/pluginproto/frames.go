// Package pluginproto defines the plugin wire protocol.
package pluginproto

import "encoding/json"

const (
	V1           = "v1"
	V2           = "v2"
	MaxLine      = 4 << 20
	MaxEventText = 256 << 10
	MaxExcerpt   = 2048
)

const (
	TypeHello  = "hello"
	TypeCall   = "call"
	TypeCancel = "cancel"
	TypeReady  = "ready"
	TypeEvent  = "event"
	TypeResult = "result"
	TypeError  = "error"
)

const (
	StatusOK       = "ok"
	StatusFailed   = "failed"
	StatusCanceled = "canceled"
)

const (
	KindInfo     = "info"
	KindProgress = "progress"
)

const (
	CodeUnsupportedProtocol = "unsupported_protocol"
	CodeBadRequest          = "bad_request"
	CodeUnsupported         = "unsupported"
	CodeBusy                = "busy"
	CodeInternal            = "internal"
	CodeRateLimited         = "rate_limited"
	CodeUnauthorised        = "unauthorised"
)

const (
	ExitOK      = 0
	ExitFailed  = 1
	ExitRefused = 2
)

type Program struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type Limits struct {
	MaxLine int `json:"max_line,omitempty"`
}

type Hello struct {
	Type      string   `json:"type"`
	Protocols []string `json:"protocols"`
	Host      Program  `json:"host"`
	Limits    *Limits  `json:"limits,omitempty"`
}

type Call struct {
	Type       string          `json:"type"`
	ID         string          `json:"id"`
	Capability string          `json:"capability"`
	Input      json.RawMessage `json:"input,omitempty"`
	TimeoutMS  int64           `json:"timeout_ms,omitempty"`
}

type Cancel struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type Capability struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type Ready struct {
	V            int          `json:"v"`
	Seq          int64        `json:"seq"`
	Type         string       `json:"type"`
	Protocol     string       `json:"protocol"`
	Plugin       Program      `json:"plugin"`
	Capabilities []Capability `json:"capabilities,omitzero"`
	Kind         string       `json:"kind,omitempty"`
	Channel      *Channel     `json:"channel,omitempty"`
}

type Event struct {
	V    int    `json:"v"`
	Seq  int64  `json:"seq"`
	Type string `json:"type"`
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Text string `json:"text"`
}

type Problem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Evidence struct {
	Kind    string `json:"kind,omitempty"`
	Source  string `json:"source,omitempty"`
	Hash    string `json:"hash,omitempty"`
	Excerpt string `json:"excerpt,omitempty"`
}

type Result struct {
	V        int             `json:"v"`
	Seq      int64           `json:"seq"`
	Type     string          `json:"type"`
	ID       string          `json:"id"`
	Status   string          `json:"status"`
	Output   json.RawMessage `json:"output,omitempty"`
	Evidence []Evidence      `json:"evidence,omitempty"`
	Error    *Problem        `json:"error,omitempty"`
}

type Error struct {
	V         int      `json:"v"`
	Seq       int64    `json:"seq"`
	Type      string   `json:"type"`
	Code      string   `json:"code"`
	Message   string   `json:"message"`
	ID        string   `json:"id,omitempty"`
	Protocols []string `json:"protocols,omitempty"`
}
