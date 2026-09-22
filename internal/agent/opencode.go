package agent

import (
	"encoding/json"
	"time"

	"github.com/cristiangirlea/harness/internal/tools"
)

// ---- opencode -------------------------------------------------------------
func NewOpencodeBackend() *CLIBackend {
	return &CLIBackend{
		name:       "opencode",
		candidates: func() []string { return npmShim("opencode") },
		// Windows .cmd shims mangle multi-line arguments, so the prompt travels
		// as an attached file.
		args: func(t Task, promptFile string) []string {
			// The message goes first: -f is an array flag and would swallow it.
			a := []string{"run", "Follow the instructions in the attached file.", "--format", "json", "--dir", t.Dir}
			if t.Model != "" {
				a = append(a, "-m", t.Model)
			}
			if t.Mode == ModeYolo {
				a = append(a, "--auto")
			}
			return append(a, "-f", promptFile)
		},
		parse: parseOpencodeStream,
	}
}
func parseOpencodeStream(line []byte, emit func(Event), res *Result) bool {
	var l struct {
		Type string `json:"type"`
		Part struct {
			Type  string `json:"type"`
			Text  string `json:"text"`
			Tool  string `json:"tool"`
			State struct {
				Status string          `json:"status"`
				Input  json.RawMessage `json:"input"`
				Output string          `json:"output"`
				Error  string          `json:"error"`
			} `json:"state"`
			Tokens struct {
				Input  int64 `json:"input"`
				Output int64 `json:"output"`
				Cache  struct {
					Read  int64 `json:"read"`
					Write int64 `json:"write"`
				} `json:"cache"`
			} `json:"tokens"`
			Cost float64 `json:"cost"`
		} `json:"part"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(line, &l) != nil || l.Type == "" {
		return false
	}
	now := time.Now()
	switch l.Type {
	case "text":
		emit(Event{Time: now, Kind: EventText, Text: l.Part.Text})
		res.Final = l.Part.Text
	case "reasoning":
		emit(Event{Time: now, Kind: EventThinking, Text: l.Part.Text})
	case "tool_use":
		emit(Event{Time: now, Kind: EventToolCall, Tool: l.Part.Tool, Input: l.Part.State.Input, Text: tools.InputSummary(l.Part.State.Input)})
		if l.Part.State.Status == "completed" || l.Part.State.Status == "error" {
			out := l.Part.State.Output
			if out == "" {
				out = l.Part.State.Error
			}
			emit(Event{Time: now, Kind: EventToolResult, Tool: l.Part.Tool, Text: out, IsError: l.Part.State.Status == "error"})
		}
	case "step_finish":
		res.Turns++
		res.InputTokens += l.Part.Tokens.Input
		res.OutputTokens += l.Part.Tokens.Output
		res.CacheRead += l.Part.Tokens.Cache.Read
		res.CacheWrite += l.Part.Tokens.Cache.Write
		res.CostUSD += l.Part.Cost
	case "error":
		msg := string(l.Error)
		emit(Event{Time: now, Kind: EventError, Text: msg})
		res.Status, res.Error = StatusError, msg
	}
	return true
}
