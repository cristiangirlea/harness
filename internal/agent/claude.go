package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/tools"
)

// ---- claude ---------------------------------------------------------------
func NewClaudeBackend() *CLIBackend {
	return &CLIBackend{
		name: "claude",
		candidates: func() []string {
			home, _ := os.UserHomeDir()
			c := []string{
				filepath.Join(home, ".local", "bin", "claude.exe"),
				filepath.Join(home, ".local", "bin", "claude"),
				filepath.Join(home, ".claude", "local", "claude"),
			}
			c = append(c, npmShim("claude")...)
			// The Claude desktop app ships its own Claude Code build.
			if runtime.GOOS == "windows" {
				c = append(c, newestVersionDir(appData("Claude", "claude-code"), "claude.exe")...)
			}
			return c
		},
		promptStdin: true,
		args: func(t Task, _ string) []string {
			a := []string{"-p", "--output-format", "stream-json", "--verbose"}
			if t.Model != "" {
				a = append(a, "--model", t.Model)
			}
			if t.Effort != "" {
				a = append(a, "--effort", t.Effort)
			}
			if t.MaxTurns > 0 {
				a = append(a, "--max-turns", strconv.Itoa(t.MaxTurns))
			}
			mode := map[Mode]string{ModeReadOnly: "plan", ModeAsk: "default", ModeAuto: "acceptEdits", ModeYolo: "bypassPermissions"}[t.Mode]
			if mode != "" {
				a = append(a, "--permission-mode", mode)
			}
			if !t.AllowDangerous {
				a = append(append(a, "--disallowedTools"), claudeDenyRules()...)
			}
			for _, p := range t.MCP {
				a = append(a, "--mcp-config", p)
			}
			if t.ForkSession != "" {
				a = append(a, "--resume", t.ForkSession, "--fork-session")
			}
			return a
		},
		parse:        parseClaudeStream,
		forks:        true,
		mapsEffort:   true,
		mapsModes:    []Mode{ModeReadOnly, ModeAuto, ModeYolo},
		mapsModel:    true,
		mapsMaxTurns: true,
		mapsMCP:      true,
		limits:       true,
		loginEnv:     []string{"CLAUDE_CODE_OAUTH_TOKEN"},
		fileRules:    true,
	}
}
func newestVersionDir(root, exe string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return versionLess(dirs[j], dirs[i]) })
	var out []string
	for _, d := range dirs {
		out = append(out, filepath.Join(root, d, exe))
	}
	return out
}
func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}

type claudeLine struct {
	Type            string  `json:"type"`
	Subtype         string  `json:"subtype"`
	Model           string  `json:"model"`
	SessionID       string  `json:"session_id"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
	Message         struct {
		Usage   apiUsage `json:"usage"`
		Model   string   `json:"model"`
		Content []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Thinking  string          `json:"thinking"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			Content   json.RawMessage `json:"content"`
			IsError   bool            `json:"is_error"`
			ToolUseID string          `json:"tool_use_id"`
		} `json:"content"`
	} `json:"message"`
	Result       string  `json:"result"`
	IsError      bool    `json:"is_error"`
	NumTurns     int     `json:"num_turns"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Usage        struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

func parseClaudeStream(line []byte, emit func(Event), res *Result) bool {
	var l claudeLine
	// A field of an unexpected type (a permission_denied system line carries
	// its message as a string, not an object) leaves the rest of the line
	// decoded, so it is read like any other; anything else that is not JSON
	// with a type is plain text.
	var mistyped *json.UnmarshalTypeError
	if err := json.Unmarshal(line, &l); (err != nil && !errors.As(err, &mistyped)) || l.Type == "" {
		return false
	}
	// Every line carries the session id; with --fork-session it is the new one.
	if l.SessionID != "" {
		res.Session = l.SessionID
	}
	now := time.Now()
	switch l.Type {
	case "system":
		if l.Subtype == "init" && l.Model != "" {
			res.Model = l.Model
			res.ModelReported = true
		}
	case "assistant", "user":
		if l.Type == "assistant" && l.ParentToolUseID == nil {
			u := l.Message.Usage
			res.ContextTokens = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
		}
		for _, c := range l.Message.Content {
			switch c.Type {
			case "text":
				emit(Event{Time: now, Kind: EventText, Text: c.Text})
			case "thinking":
				if c.Thinking != "" {
					emit(Event{Time: now, Kind: EventThinking, Text: c.Thinking})
				}
			case "tool_use":
				emit(Event{Time: now, Kind: EventToolCall, Tool: c.Name, Input: c.Input, Text: tools.InputSummary(c.Input)})
			case "tool_result":
				emit(Event{Time: now, Kind: EventToolResult, Text: contentText(c.Content), IsError: c.IsError})
			}
		}
	case "result":
		res.Final = l.Result
		res.Turns = l.NumTurns
		res.CostUSD = l.TotalCostUSD
		res.InputTokens = l.Usage.InputTokens
		res.OutputTokens = l.Usage.OutputTokens
		res.CacheRead = l.Usage.CacheReadInputTokens
		res.CacheWrite = l.Usage.CacheCreationInputTokens
		res.Status = StatusOK
		if l.IsError {
			res.Status = StatusError
			// The subtype classifies the failure and the result text says what
			// happened; a login failure arrives as subtype "success" with only
			// the text. For max_turns the text is the partial answer, not a reason.
			switch {
			case l.Subtype == "success" && l.Result != "":
				res.Error = l.Result
			case l.Subtype == "success":
				res.Error = "unknown error"
			case l.Result != "" && l.Subtype != "error_max_turns":
				res.Error = l.Subtype + ": " + l.Result
			default:
				res.Error = l.Subtype
			}
			if l.Subtype == "error_max_turns" {
				res.Status = StatusMaxTurns
			}
		}
	}
	return true
}
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, bl := range blocks {
			b.WriteString(bl.Text)
		}
		return b.String()
	}
	return string(raw)
}
