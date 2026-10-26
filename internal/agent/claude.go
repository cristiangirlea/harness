package agent

import (
	"encoding/json"
	"errors"
	"math"
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

const appendSystemPromptFlag = "--append-system-prompt"

// NewClaudeBackend drives the Claude Code CLI.
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
			if t.IsLean() {
				// Limit MCP servers without losing settings or subscription login; --bare requires an API key.
				a = append(a, "--strict-mcp-config")
			}
			if t.CompactWindow != "" {
				a = append(a, "--autocompact", string(t.CompactWindow))
			}
			if t.AppendSystemPrompt != "" {
				// One argument, so text starting with a dash is never read as a flag.
				a = append(a, appendSystemPromptFlag+"="+t.AppendSystemPrompt)
			}
			if t.NoSkills || (t.IsLean() && (t.Skills == nil || !*t.Skills)) {
				a = append(a, "--disable-slash-commands")
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
		parse:            parseClaudeStream,
		newParse:         newClaudeParse,
		stopWhen:         func(r *Result) bool { return strings.HasPrefix(r.Error, perTokenRefusal) },
		forks:            true,
		mapsEffort:       true,
		mapsModes:        []Mode{ModeReadOnly, ModeAuto, ModeYolo},
		mapsModel:        true,
		mapsMaxTurns:     true,
		mapsMCP:          true,
		mapsLean:         true,
		mapsCompact:      true,
		mapsSystemPrompt: true,
		mapsNoSkills:     true,
		limits:           true,
		loginEnv:         []string{"CLAUDE_CODE_OAUTH_TOKEN"},
		fileRules:        true,
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
	RateLimitInfo struct {
		RateLimitType  string   `json:"rateLimitType"`
		Utilization    *float64 `json:"utilization"`
		ResetsAt       int64    `json:"resetsAt"`
		UnifiedWindows struct {
			FiveHour *claudeWindow `json:"five_hour"`
			SevenDay *claudeWindow `json:"seven_day"`
		} `json:"unifiedWindows"`
	} `json:"rate_limit_info"`
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

// Message lines repeat usage; emit its largest output once, flushing on nil.
func newClaudeParse() func([]byte, func(Event), *Result) bool {
	m := &claudeMessage{seen: map[string]bool{}}
	return func(line []byte, emit func(Event), res *Result) bool {
		if line == nil {
			m.flush(emit)
			return false
		}
		return parseClaudeLine(line, emit, res, m)
	}
}

type claudeMessage struct {
	id      string
	pending *Event
	seen    map[string]bool
}

func (m *claudeMessage) flush(emit func(Event)) {
	if m.pending != nil {
		emit(*m.pending)
	}
	m.id, m.pending = "", nil
}

func parseClaudeStream(line []byte, emit func(Event), res *Result) bool {
	return parseClaudeLine(line, emit, res, nil)
}

func parseClaudeLine(line []byte, emit func(Event), res *Result, m *claudeMessage) bool {
	var l claudeLine
	// Permission-denied lines may mistype message; keep other decoded fields.
	var mistyped *json.UnmarshalTypeError
	if err := json.Unmarshal(line, &l); (err != nil && !errors.As(err, &mistyped)) || l.Type == "" {
		return false
	}
	mainLoop := l.Type == "assistant" && l.ParentToolUseID == nil
	if m != nil && (!mainLoop || l.Message.ID != m.id) {
		m.flush(emit)
	}
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
		// Sub-agent usage belongs in run totals, not main-loop context.
		if mainLoop {
			u := l.Message.Usage
			res.ContextTokens = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
			id := l.Message.ID
			switch {
			case u == (apiUsage{}):
			case m != nil && m.pending != nil && id == m.id:
				m.pending.Usage.OutputTokens = max(m.pending.Usage.OutputTokens, u.OutputTokens)
			case m != nil && id != "" && m.seen[id]:
			default:
				e := Event{Time: now, Kind: EventUsage, Usage: &Usage{Model: l.Message.Model,
					InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
					CacheRead: u.CacheReadInputTokens, CacheWrite: u.CacheCreationInputTokens, ContextTokens: res.ContextTokens}}
				if m == nil || id == "" {
					emit(e)
					break
				}
				m.seen[id] = true
				m.id, m.pending = id, &e
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
	case "rate_limit_event":
		if q := claudeQuota(l); q != nil {
			emit(Event{Time: now, Kind: EventQuota, Quota: q})
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
			// Login failures may say success; max_turns text is a partial answer.
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

// claudeWindow is one window of a rate_limit_event: utilization is the
// fraction used, 0 to 1, and resetsAt is in Unix seconds.
type claudeWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    int64   `json:"resetsAt"`
}

func (w *claudeWindow) quota() *QuotaWindow {
	if w == nil || w.ResetsAt <= 0 || math.IsNaN(w.Utilization) || math.IsInf(w.Utilization, 0) || w.Utilization < 0 {
		return nil
	}
	return &QuotaWindow{UsedPercent: math.Round(w.Utilization*10000) / 100, ResetsAt: w.ResetsAt}
}

// claudeQuota reads a rate_limit_event: its per-window figures when it carries
// them, otherwise the one window the event itself is about.
func claudeQuota(l claudeLine) *Quota {
	info := l.RateLimitInfo
	q := Quota{FiveHour: info.UnifiedWindows.FiveHour.quota(), Week: info.UnifiedWindows.SevenDay.quota()}
	if q.FiveHour == nil && q.Week == nil && info.Utilization != nil {
		w := &claudeWindow{Utilization: *info.Utilization, ResetsAt: info.ResetsAt}
		switch info.RateLimitType {
		case "five_hour":
			q.FiveHour = w.quota()
		case "seven_day":
			q.Week = w.quota()
		}
	}
	if q.FiveHour == nil && q.Week == nil {
		return nil
	}
	return &q
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
