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

const perTokenRefusal = "claude would bill per token"

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
			if !t.InheritEnv {
				// Only the repository's own settings: the operator's user and
				// local settings, with their hooks, plugins and permission
				// rules, stay out of an unattended run (#411).
				a = append(a, "--setting-sources", "project")
			}
			if t.IsLean() || !t.InheritEnv {
				// Only the --mcp-config files below: none of the user's other
				// MCP servers, plugin servers or claude.ai connectors. Lean on
				// its own keeps every setting source, so the user's CLAUDE.md,
				// rules, hooks, plugins and skills load too (#204). Not --bare:
				// it logs in only with an API key, never the subscription.
				a = append(a, "--strict-mcp-config")
			}
			if t.CompactWindow != "" {
				a = append(a, "--autocompact", string(t.CompactWindow))
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
		newParse:     newClaudeParse,
		stopWhen:     func(r *Result) bool { return strings.HasPrefix(r.Error, perTokenRefusal) },
		forks:        true,
		mapsEffort:   true,
		mapsModes:    []Mode{ModeReadOnly, ModeAuto, ModeYolo},
		mapsModel:    true,
		mapsMaxTurns: true,
		mapsMCP:      true,
		mapsLean:     true,
		mapsCompact:  true,
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
	APIKeySource    string  `json:"apiKeySource"`
	Model           string  `json:"model"`
	SessionID       string  `json:"session_id"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
	Message         struct {
		ID      string   `json:"id"`
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
	CompactMetadata struct {
		Trigger   string `json:"trigger"`
		PreTokens int64  `json:"pre_tokens"`
	} `json:"compact_metadata"`
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

// newClaudeParse is parseClaudeStream for one run. stream-json writes each
// content block of a message as a line of its own and repeats the message's
// usage on every one, so the usage event goes out once per message id, with
// the first line's figures.
func newClaudeParse() func([]byte, func(Event), *Result) bool {
	seen := map[string]bool{}
	return func(line []byte, emit func(Event), res *Result) bool {
		return parseClaudeLine(line, emit, res, seen)
	}
}

// parseClaudeStream reads one line on its own: a line with usage is a usage
// event, whatever came before.
func parseClaudeStream(line []byte, emit func(Event), res *Result) bool {
	return parseClaudeLine(line, emit, res, nil)
}

func parseClaudeLine(line []byte, emit func(Event), res *Result, seen map[string]bool) bool {
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
		if l.Subtype == "init" && l.APIKeySource != "" && l.APIKeySource != "none" {
			res.Status = StatusError
			res.Error = perTokenRefusal + " (apiKeySource " + strconv.Quote(l.APIKeySource) + "): refused; hrn runs claude on the plan login only"
			return true
		}
		if l.Subtype == "init" && l.Model != "" {
			res.Model = l.Model
			res.ModelReported = true
		}
		if l.Subtype == "compact_boundary" {
			emit(Event{Time: now, Kind: EventCompaction, Text: l.CompactMetadata.Trigger,
				Usage: &Usage{ContextTokens: l.CompactMetadata.PreTokens}})
		}
	case "assistant", "user":
		// A sub-agent's turns (under a parent tool call) fill its own context,
		// not the main loop's: they count in the run's totals only.
		if l.Type == "assistant" && l.ParentToolUseID == nil {
			u := l.Message.Usage
			res.ContextTokens = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
			id := l.Message.ID
			if u != (apiUsage{}) && (id == "" || !seen[id]) {
				if seen != nil && id != "" {
					seen[id] = true
				}
				emit(Event{Time: now, Kind: EventUsage, Usage: &Usage{Model: l.Message.Model,
					InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
					CacheRead: u.CacheReadInputTokens, CacheWrite: u.CacheCreationInputTokens, ContextTokens: res.ContextTokens}})
			}
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
