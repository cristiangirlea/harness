package agent

import (
	"encoding/json"
	"time"
)

// ---- codex ----------------------------------------------------------------
func NewCodexBackend() *CLIBackend {
	return &CLIBackend{
		name:        "codex",
		candidates:  func() []string { return npmShim("codex") },
		promptStdin: true,
		args: func(t Task, _ string) []string {
			a := []string{"exec", "--json", "--skip-git-repo-check", "-C", t.Dir}
			if t.Model != "" {
				a = append(a, "-m", t.Model)
			}
			switch t.Mode {
			case ModeReadOnly, ModeAsk:
				a = append(a, "--sandbox", "read-only")
			case ModeAuto:
				a = append(a, "--full-auto")
			case ModeYolo:
				a = append(a, "--dangerously-bypass-approvals-and-sandbox")
			}
			return append(a, "-")
		},
		parse: parseCodexStream,
	}
}
func parseCodexStream(line []byte, emit func(Event), res *Result) bool {
	var l struct {
		Type string `json:"type"`
		Item struct {
			Type             string `json:"type"`
			Text             string `json:"text"`
			Command          string `json:"command"`
			AggregatedOutput string `json:"aggregated_output"`
			ExitCode         *int   `json:"exit_code"`
		} `json:"item"`
		Usage struct {
			InputTokens       int64 `json:"input_tokens"`
			CachedInputTokens int64 `json:"cached_input_tokens"`
			OutputTokens      int64 `json:"output_tokens"`
		} `json:"usage"`
		Message string `json:"message"`
	}
	if json.Unmarshal(line, &l) != nil || l.Type == "" {
		return false
	}
	now := time.Now()
	switch l.Type {
	case "item.started":
		if l.Item.Type == "command_execution" {
			emit(Event{Time: now, Kind: EventToolCall, Tool: "shell", Text: l.Item.Command})
		}
	case "item.completed":
		switch l.Item.Type {
		case "agent_message":
			emit(Event{Time: now, Kind: EventText, Text: l.Item.Text})
			res.Final = l.Item.Text
		case "reasoning":
			emit(Event{Time: now, Kind: EventThinking, Text: l.Item.Text})
		case "command_execution":
			emit(Event{Time: now, Kind: EventToolResult, Tool: "shell", Text: l.Item.AggregatedOutput,
				IsError: l.Item.ExitCode != nil && *l.Item.ExitCode != 0})
		}
	case "turn.completed":
		res.Turns++
		res.InputTokens += l.Usage.InputTokens
		res.CacheRead += l.Usage.CachedInputTokens
		res.OutputTokens += l.Usage.OutputTokens
	case "error", "turn.failed":
		emit(Event{Time: now, Kind: EventError, Text: l.Message})
		res.Status, res.Error = StatusError, l.Message
	}
	return true
}
