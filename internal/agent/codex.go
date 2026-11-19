package agent

import (
	"bufio"
	"cmp"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
)

// NewCodexBackend drives the Codex CLI.
func NewCodexBackend() *CLIBackend {
	return &CLIBackend{
		name: "codex",
		candidates: func() []string {
			c := npmShim("codex")
			if runtime.GOOS == "windows" {
				c = append(c, newestFile(filepath.Join(os.Getenv("LOCALAPPDATA"), "OpenAI", "Codex", "bin"), "codex.exe")...)
			}
			return c
		},
		promptStdin: true,
		args: func(t Task, _ string) []string {
			// exec's options go before a fork subcommand, which applies them.
			a := []string{"exec", "--json", "--skip-git-repo-check", "-C", t.Dir}
			if t.Model != "" {
				a = append(a, "-m", t.Model)
			}
			if t.Effort != "" {
				a = append(a, "-c", "model_reasoning_effort="+t.Effort)
			}
			a = append(a, codexMCPArgs(t)...)
			switch t.Mode {
			case ModeReadOnly, ModeAsk:
				a = append(a, "--sandbox", "read-only")
			case ModeAuto:
				// Exec cannot prompt, so its sandbox must enforce the policy.
				a = append(a, "--sandbox", "workspace-write")
			case ModeYolo:
				a = append(a, "--dangerously-bypass-approvals-and-sandbox")
			}
			if t.ForkSession != "" {
				a = append(a, "fork", t.ForkSession)
			}
			return append(a, "-")
		},
		env: codexMCPEnv,
		mcpWarnings: func(t Task) []string {
			_, _, skipped, _ := codexMCPConfigs(t)
			var warnings []string
			for _, name := range skipped {
				warnings = append(warnings, "mcp "+name+": skipped; name is not a bare key or collides with codex config")
			}
			return warnings
		},
		gateRoute:    codexGateRoute,
		newGateParse: newCodexGateParse,
		gateArgsAt:   codexGateArgsAt,
		gateLine: func(gate string) string {
			return "gate: codex's shell tools are off and its commands go to hrn's run_command, through " + gate + "; MCP servers' tools are not routed"
		},
		stopWhen:   func(r *Result) bool { return strings.HasPrefix(r.Error, gateUnrouted) },
		parse:      parseCodexStream,
		end:        codexStreamEnded,
		exited:     codexReportModel,
		forks:      true,
		mapsEffort: true,
		mapsModes:  []Mode{ModeReadOnly, ModeAuto, ModeYolo},
		mapsModel:  true,
		mapsMCP:    true,
		mapsLean:   true,
		limits:     true,
	}
}

// codexReportModel takes the model from the session's rollout file: exec --json names none.
// It also adds the tool calls the stream never showed, when the rollout's tool items match the stream's.
func codexReportModel(t Task, started time.Time, res *Result, emit func(Event), streamed int) {
	r := codexRollout(codexHomeDir(t), res.Session, started, time.Now())
	res.CLIVersion = r.version
	if r.counted == streamed {
		for _, e := range r.calls {
			emit(e)
		}
	}
	if r.model == "" {
		return
	}
	res.Model, res.ModelReported = r.model, true
	if res.Usage != nil {
		estimateCodexCost(res)
	}
}

// codexRolloutRead is what a session's rollout file adds to the stream.
type codexRolloutRead struct {
	model, version string
	// counted is how many of this run's items the stream also reports as tool calls.
	counted int
	// calls are this run's tool calls that ran none of those items.
	calls []Event
}

// The rollout items whose stream counterparts parseCodexStream counts as tool calls.
var codexStreamedTools = []string{"CommandExecution", "McpToolCall", "FileChange"}

// codexRollout reads what codex recorded for session: the last turn's model, the CLI version and the unstreamed calls.
func codexRollout(home, session string, started, now time.Time) codexRolloutRead {
	if home == "" || !sessionID.MatchString(session) {
		return codexRolloutRead{}
	}
	// Folders are named by the session's local start date; a day either side covers time zones.
	first := started.AddDate(0, 0, -1)
	first = time.Date(first.Year(), first.Month(), first.Day(), 0, 0, 0, 0, first.Location())
	for day := first; !day.After(now.AddDate(0, 0, 1)); day = day.AddDate(0, 0, 1) {
		dir := filepath.Join(home, "sessions", filepath.FromSlash(day.Format("2006/01/02")))
		// ReadDir, not Glob: a home path may hold pattern characters such as [.
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, "-"+session+".jsonl") {
				continue
			}
			if r, ok := readCodexRollout(filepath.Join(dir, name), session, started); ok {
				return r
			}
		}
	}
	return codexRolloutRead{}
}

// readCodexRollout counts only lines stamped since the run started, so an earlier run's calls stay out.
func readCodexRollout(path, session string, since time.Time) (r codexRolloutRead, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return r, false
	}
	defer f.Close()
	type call struct {
		name, input, output string
		start, end          time.Time
		done, ranTool       bool
	}
	var calls []*call
	byID := map[string]*call{}
	var itemStarts []time.Time
	placeable := true
	rd := bufio.NewReader(f)
	for {
		raw, err := rd.ReadBytes('\n')
		if len(raw) == 0 && err != nil {
			break
		}
		var line struct {
			Timestamp string          `json:"timestamp"`
			Type      string          `json:"type"`
			Payload   json.RawMessage `json:"payload"`
		}
		// A line codex cut short when it was killed is skipped, not fatal.
		if json.Unmarshal(raw, &line) != nil {
			continue
		}
		switch line.Type {
		case "session_meta", "turn_context":
			var p struct {
				ID         string `json:"id"`
				Model      string `json:"model"`
				CLIVersion string `json:"cli_version"`
			}
			if line.Type == "session_meta" && ok || json.Unmarshal(line.Payload, &p) != nil {
				continue
			}
			if line.Type == "turn_context" {
				if ValidModel(p.Model) {
					r.model = p.Model
				}
				continue
			}
			// The first session_meta names the file's own session; later ones are ignored.
			if p.ID != session {
				return codexRolloutRead{}, false
			}
			ok = true
			if ValidModel(p.CLIVersion) {
				r.version = p.CLIVersion
			}
		case "response_item", "event_msg":
			at, err := time.Parse(time.RFC3339Nano, line.Timestamp)
			if !ok || err != nil || at.Before(since) {
				continue
			}
			var p struct {
				Type        string          `json:"type"`
				CallID      string          `json:"call_id"`
				Name        string          `json:"name"`
				Input       string          `json:"input"`
				Arguments   string          `json:"arguments"`
				Output      json.RawMessage `json:"output"`
				StartedAtMS int64           `json:"started_at_ms"`
				Item        struct {
					Type string `json:"type"`
				} `json:"item"`
			}
			if json.Unmarshal(line.Payload, &p) != nil {
				continue
			}
			switch p.Type {
			case "custom_tool_call", "function_call":
				if byID[p.CallID] == nil {
					c := &call{name: p.Name, input: cmp.Or(p.Input, p.Arguments), start: at}
					calls = append(calls, c)
					byID[p.CallID] = c
				}
			case "item_completed":
				if !slices.Contains(codexStreamedTools, p.Item.Type) {
					continue
				}
				r.counted++
				if p.StartedAtMS <= 0 {
					placeable = false
				}
				itemStarts = append(itemStarts, time.UnixMilli(p.StartedAtMS))
			case "custom_tool_call_output", "function_call_output":
				if c := byID[p.CallID]; c != nil && !c.done {
					c.done, c.end, c.output = true, at, codexOutputText(p.Output)
				}
			}
		}
	}
	// An item belongs to the call running when it started; its completion may come after the call returned.
	for _, started := range itemStarts {
		placed := false
		for _, c := range calls {
			if !started.Before(c.start) && (!c.done || !started.After(c.end)) {
				c.ranTool, placed = true, true
			}
		}
		placeable = placeable && placed
	}
	if !placeable {
		return r, ok
	}
	for _, c := range calls {
		if c.ranTool {
			continue
		}
		r.calls = append(r.calls, Event{Time: c.start, Kind: EventToolCall, Tool: c.name, Text: c.input})
		if c.done {
			r.calls = append(r.calls, Event{Time: c.end, Kind: EventToolResult, Tool: c.name, Text: c.output,
				IsError: strings.HasPrefix(c.output, "Script failed")})
		}
	}
	return r, ok
}

// codexOutputText reads a call's output, which codex stores as a string or as text parts.
func codexOutputText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, part := range parts {
			b.WriteString(part.Text)
		}
		return b.String()
	}
	return string(raw)
}

// Hashed build names require ordering by modification time.
func newestFile(root, name string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	type found struct {
		path string
		mod  time.Time
	}
	var files []found
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(root, e.Name(), name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			files = append(files, found{p, info.ModTime()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.path
	}
	return out
}

func parseCodexStream(line []byte, emit func(Event), res *Result) bool {
	return parseCodexLine(line, emit, res, false)
}

// parseCodexLine names the gate's tools as the gate's only when gated: elsewhere a server may share its name.
func parseCodexLine(line []byte, emit func(Event), res *Result, gated bool) bool {
	var l struct {
		Type     string `json:"type"`
		ThreadID string `json:"thread_id"`
		Item     struct {
			Type             string `json:"type"`
			Text             string `json:"text"`
			Command          string `json:"command"`
			AggregatedOutput string `json:"aggregated_output"`
			ExitCode         *int   `json:"exit_code"`
		} `json:"item"`
		Usage *struct {
			InputTokens       *int64 `json:"input_tokens"`
			CachedInputTokens *int64 `json:"cached_input_tokens"`
			OutputTokens      *int64 `json:"output_tokens"`
		} `json:"usage"`
		Message string `json:"message"`
		// Error carries turn.failed diagnostics outside Message.
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(line, &l) != nil || l.Type == "" {
		return false
	}
	now := time.Now()
	switch l.Type {
	case "thread.started":
		res.Session = l.ThreadID
	case "item.started":
		switch l.Item.Type {
		case "command_execution":
			emit(Event{Time: now, Kind: EventToolCall, Tool: "shell", Text: l.Item.Command})
		case "file_change":
			var changes []string
			for _, c := range readCodexToolItem(line).Changes {
				changes = append(changes, c.Kind+" "+c.Path)
			}
			emit(Event{Time: now, Kind: EventToolCall, Tool: "apply_patch", Text: strings.Join(changes, "\n")})
		case "mcp_tool_call":
			item := readCodexToolItem(line).codexMCPItem
			emit(Event{Time: now, Kind: EventToolCall, Tool: codexMCPToolName(item, gated), Text: codexGateCallText(item)})
		}
	case "item.completed":
		switch l.Item.Type {
		case "file_change":
			emit(Event{Time: now, Kind: EventToolResult, Tool: "apply_patch", IsError: readCodexToolItem(line).Status == "failed"})
		case "mcp_tool_call":
			item := readCodexToolItem(line).codexMCPItem
			emit(Event{Time: now, Kind: EventToolResult, Tool: codexMCPToolName(item, gated), Text: codexGateResultText(item),
				IsError: item.Error != nil || item.Status == "failed"})
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
		if codexReconnecting(res.Error) {
			res.Status, res.Error = "", ""
		}
		if l.Usage == nil || l.Usage.InputTokens == nil || l.Usage.CachedInputTokens == nil || l.Usage.OutputTokens == nil {
			res.Usage = &UsageMetadata{InputSemantics: "inclusive_cache", Status: "missing", CostStatus: "missing"}
		} else {
			if res.Usage == nil {
				res.Usage = &UsageMetadata{InputSemantics: "inclusive_cache", Status: "reported", CostStatus: "missing"}
			}
			for _, counter := range [][2]int64{
				{res.InputTokens, *l.Usage.InputTokens},
				{res.CacheRead, *l.Usage.CachedInputTokens},
				{res.OutputTokens, *l.Usage.OutputTokens},
			} {
				if counter[0] < 0 || counter[1] < 0 || counter[1] > math.MaxInt64-counter[0] {
					res.Usage.Status = "missing"
					estimateCodexCost(res)
					return true
				}
			}
			res.InputTokens += *l.Usage.InputTokens
			res.CacheRead += *l.Usage.CachedInputTokens
			res.OutputTokens += *l.Usage.OutputTokens
		}
		estimateCodexCost(res)
	case "error", "turn.failed":
		msg := cmp.Or(l.Message, l.Error.Message)
		if l.Type == "error" && codexReconnecting(msg) {
			// Reconnect notices remain provisional; preserve earlier errors.
			emit(Event{Time: now, Kind: EventInfo, Text: msg})
			if res.Error == "" || codexReconnecting(res.Error) {
				res.Status, res.Error = StatusError, msg
			}
			break
		}
		res.Usage = &UsageMetadata{InputSemantics: "inclusive_cache", Status: "missing", CostStatus: "missing"}
		estimateCodexCost(res)
		if msg == "" || msg == res.Error {
			// Keep the failure text needed for display and plan-limit detection.
			if res.Error != "" && !codexReconnecting(res.Error) {
				break
			}
			msg = cmp.Or(res.Error, l.Type)
		}
		emit(Event{Time: now, Kind: EventError, Text: msg})
		res.Status, res.Error = StatusError, msg
	}
	return true
}

// Exit during reconnect means failure and missing turn usage.
func codexStreamEnded(emit func(Event), res *Result, errored bool) {
	if res.Status != StatusError || !codexReconnecting(res.Error) {
		return
	}
	res.Usage = &UsageMetadata{InputSemantics: "inclusive_cache", Status: "missing", CostStatus: "missing"}
	estimateCodexCost(res)
	if !errored {
		emit(Event{Time: time.Now(), Kind: EventError, Text: res.Error})
	}
}

func codexReconnecting(msg string) bool {
	return strings.HasPrefix(msg, "Reconnecting...")
}
