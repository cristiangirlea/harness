package agent

import (
	"bufio"
	"cmp"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
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
func codexReportModel(t Task, started time.Time, res *Result) {
	model, version := codexRollout(codexHomeDir(t.Dir), res.Session, started, time.Now())
	res.CLIVersion = version
	if model == "" {
		return
	}
	res.Model, res.ModelReported = model, true
	if res.Usage != nil {
		estimateCodexCost(res)
	}
}

// codexRollout reads the last turn's model and the CLI version codex recorded for session.
func codexRollout(home, session string, started, now time.Time) (model, version string) {
	if home == "" || !sessionID.MatchString(session) {
		return "", ""
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
			if model, version, ok := readCodexRollout(filepath.Join(dir, name), session); ok {
				return model, version
			}
		}
	}
	return "", ""
}

func readCodexRollout(path, session string) (model, version string, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", false
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for {
		raw, err := r.ReadBytes('\n')
		if len(raw) == 0 && err != nil {
			break
		}
		var line struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		// A line codex cut short when it was killed is skipped, not fatal.
		if json.Unmarshal(raw, &line) != nil {
			continue
		}
		var p struct {
			ID         string `json:"id"`
			Model      string `json:"model"`
			CLIVersion string `json:"cli_version"`
		}
		if (line.Type != "session_meta" || ok) && line.Type != "turn_context" || json.Unmarshal(line.Payload, &p) != nil {
			continue
		}
		switch line.Type {
		case "session_meta":
			// The first session_meta names the file's own session; later ones are ignored.
			if p.ID != session {
				return "", "", false
			}
			ok = true
			if ValidModel(p.CLIVersion) {
				version = p.CLIVersion
			}
		case "turn_context":
			if ValidModel(p.Model) {
				model = p.Model
			}
		}
	}
	return model, version, ok
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
