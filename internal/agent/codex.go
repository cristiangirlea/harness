package agent

import (
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

// ---- codex ----------------------------------------------------------------
func NewCodexBackend() *CLIBackend {
	return &CLIBackend{
		name: "codex",
		candidates: func() []string {
			c := npmShim("codex")
			// The Codex desktop app ships its own CLI, in one directory per build.
			if runtime.GOOS == "windows" {
				c = append(c, newestFile(filepath.Join(os.Getenv("LOCALAPPDATA"), "OpenAI", "Codex", "bin"), "codex.exe")...)
			}
			return c
		},
		promptStdin: true,
		args: func(t Task, _ string) []string {
			// exec's own options go before a fork subcommand, which applies them.
			a := []string{"exec", "--json", "--skip-git-repo-check", "-C", t.Dir}
			if t.Model != "" {
				a = append(a, "-m", t.Model)
			}
			if t.Effort != "" {
				a = append(a, "-c", "model_reasoning_effort="+t.Effort)
			}
			switch t.Mode {
			case ModeReadOnly, ModeAsk:
				a = append(a, "--sandbox", "read-only")
			case ModeAuto:
				// What --full-auto meant before codex 0.155 dropped it: exec never
				// asks, so the sandbox is the whole policy.
				a = append(a, "--sandbox", "workspace-write")
			case ModeYolo:
				a = append(a, "--dangerously-bypass-approvals-and-sandbox")
			}
			if t.ForkSession != "" {
				a = append(a, "fork", t.ForkSession)
			}
			return append(a, "-")
		},
		parse:      parseCodexStream,
		forks:      true,
		mapsEffort: true,
		mapsModes:  []Mode{ModeReadOnly, ModeAuto, ModeYolo},
		mapsModel:  true,
		limits:     true,
	}
}

// newestFile lists root/<dir>/name for every subdirectory that has one, most
// recently modified first. The Codex app names its build directories by hash,
// so their names say nothing about which is current.
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
		// turn.failed carries its text here rather than in Message.
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
		// A fork reports its new thread here, not the one it copied.
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
			// The turn survived its reconnects: the notice was not the end.
			res.Status, res.Error = "", ""
		}
		if l.Usage == nil || l.Usage.InputTokens == nil || l.Usage.CachedInputTokens == nil || l.Usage.OutputTokens == nil {
			res.Usage = &UsageMetadata{InputSemantics: "inclusive_cache", Status: "missing", CostStatus: "missing"}
		} else {
			if res.Usage == nil {
				res.Usage = &UsageMetadata{InputSemantics: "inclusive_cache", Status: "reported", CostStatus: "missing"}
			}
			// Check the whole turn before adding it; missing usage stays missing.
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
			// The CLI lost its model stream and retries by itself; the turn
			// goes on. The notice is shown as information and stands as the
			// run's error only until turn.completed, so a stream that ends on
			// it still says why. The usage the turn reports later is whole.
			emit(Event{Time: now, Kind: EventInfo, Text: msg})
			res.Status, res.Error = StatusError, msg
			break
		}
		res.Usage = &UsageMetadata{InputSemantics: "inclusive_cache", Status: "missing", CostStatus: "missing"}
		estimateCodexCost(res)
		if msg == "" || msg == res.Error {
			// A turn.failed after the error line that said why: keep that text,
			// which is what a reader and the limit check need. A reconnect
			// notice was shown as information, so the failure is reported here.
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

// codexReconnecting reports whether a codex error line is the CLI saying it is
// retrying a dropped model stream ("Reconnecting... 2/5 (...)"), which the turn
// may still survive.
func codexReconnecting(msg string) bool {
	return strings.HasPrefix(msg, "Reconnecting...")
}
