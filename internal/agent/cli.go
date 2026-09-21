package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/tools"
)

// CLIBackend drives an installed agent CLI non-interactively, so the work is
// billed to that CLI's own subscription login rather than per API token.
type CLIBackend struct {
	name string
	// candidates lists extra install locations checked after PATH.
	candidates func() []string
	// args builds the argument list. promptFile is set when the prompt is
	// passed as an attachment instead of on stdin.
	args        func(t Task, promptFile string) []string
	promptStdin bool
	// parse handles one stdout line. Nil means stdout is plain text.
	parse func(line []byte, emit func(Event), res *Result) bool
	// finalize runs after exit with all of stdout, for single-JSON outputs.
	finalize func(stdout []byte, emit func(Event), res *Result)
}

func (c *CLIBackend) Name() string { return c.name }

func (c *CLIBackend) Detect() (string, error) {
	if p, err := exec.LookPath(c.name); err == nil {
		return p, nil
	}
	if c.candidates != nil {
		for _, p := range c.candidates() {
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("%s not found on PATH or in known install locations", c.name)
}

func (c *CLIBackend) Run(ctx context.Context, task Task, emit func(Event)) Result {
	started := time.Now()
	res := Result{Backend: c.name, Model: task.Model}
	fail := func(status Status, err error) Result {
		res.Status = status
		res.Error = err.Error()
		emit(Event{Time: time.Now(), Kind: EventError, Text: err.Error()})
		return finish(&res, started)
	}

	bin, err := c.Detect()
	if err != nil {
		return fail(StatusError, err)
	}
	var promptFile string
	if !c.promptStdin {
		f, err := os.CreateTemp("", "hx-prompt-*.md")
		if err != nil {
			return fail(StatusError, err)
		}
		promptFile = f.Name()
		defer os.Remove(promptFile)
		_, err = f.WriteString(task.Prompt)
		f.Close()
		if err != nil {
			return fail(StatusError, err)
		}
	}

	cmd := exec.CommandContext(ctx, bin, c.args(task, promptFile)...)
	cmd.Dir = task.Dir
	// A nested Claude Code session refuses to start when it thinks it is
	// inside another one.
	cmd.Env = withoutEnv(os.Environ(), "CLAUDECODE")
	if c.promptStdin {
		cmd.Stdin = strings.NewReader(task.Prompt)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fail(StatusError, err)
	}
	emit(Event{Time: time.Now(), Kind: EventInfo, Text: fmt.Sprintf("%s %s", filepath.Base(bin), strings.Join(cmd.Args[1:], " "))})
	if err := cmd.Start(); err != nil {
		return fail(StatusError, err)
	}

	var all []byte
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	var plain strings.Builder
	for sc.Scan() {
		line := sc.Bytes()
		if c.finalize != nil {
			all = append(append(all, line...), '\n')
			continue
		}
		if c.parse != nil && c.parse(line, emit, &res) {
			continue
		}
		// Unparsed output is shown as text so nothing is silently lost.
		text := string(line) + "\n"
		plain.WriteString(text)
		emit(Event{Kind: EventText, Text: text, Partial: true})
	}
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()

	if c.finalize != nil {
		c.finalize(all, emit, &res)
	}
	if res.Final == "" {
		res.Final = strings.TrimSpace(plain.String())
		if res.Final != "" {
			emit(Event{Time: time.Now(), Kind: EventText, Text: res.Final, Streamed: true})
		}
	}
	switch {
	case ctx.Err() != nil:
		return fail(StatusCanceled, ctx.Err())
	case waitErr != nil:
		// Prefer what the CLI said over the bare exit status: stderr first,
		// then a structured error the stream already reported. A status the
		// stream set (max_turns, refused) survives the non-zero exit too.
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = res.Error
		}
		if msg == "" {
			msg = waitErr.Error()
		}
		status := StatusError
		if res.Status != "" && res.Status != StatusOK {
			status = res.Status
		}
		return fail(status, errors.New(lastLines(msg, 20)))
	case res.Status == "":
		res.Status = StatusOK
	}
	return finish(&res, started)
}

func withoutEnv(env []string, key string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(strings.ToUpper(kv), key+"=") {
			out = append(out, kv)
		}
	}
	return out
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func appData(parts ...string) string {
	return filepath.Join(append([]string{os.Getenv("APPDATA")}, parts...)...)
}

func npmShim(name string) []string {
	if runtime.GOOS != "windows" {
		return nil
	}
	return []string{appData("npm", name+".cmd")}
}

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
			if t.MaxTurns > 0 {
				a = append(a, "--max-turns", strconv.Itoa(t.MaxTurns))
			}
			mode := map[Mode]string{ModeReadOnly: "plan", ModeAsk: "default", ModeAuto: "acceptEdits", ModeYolo: "bypassPermissions"}[t.Mode]
			if mode != "" {
				a = append(a, "--permission-mode", mode)
			}
			for _, p := range t.MCP {
				a = append(a, "--mcp-config", p)
			}
			return a
		},
		parse: parseClaudeStream,
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
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Model   string `json:"model"`
	Message struct {
		Model   string `json:"model"`
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
	if json.Unmarshal(line, &l) != nil || l.Type == "" {
		return false
	}
	now := time.Now()
	switch l.Type {
	case "system":
		if l.Subtype == "init" && l.Model != "" {
			res.Model = l.Model
		}
	case "assistant", "user":
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
			// A specific subtype names the failure; "success" with is_error
			// (a login failure, for one) puts the reason in result instead.
			res.Error = l.Subtype
			if l.Subtype == "success" {
				res.Error = l.Result
				if res.Error == "" {
					res.Error = "unknown error"
				}
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

// ---- gemini ---------------------------------------------------------------

func NewGeminiBackend() *CLIBackend {
	return &CLIBackend{
		name:        "gemini",
		candidates:  func() []string { return npmShim("gemini") },
		promptStdin: true,
		args: func(t Task, _ string) []string {
			a := []string{"-o", "json"}
			if t.Model != "" {
				a = append(a, "-m", t.Model)
			}
			mode := map[Mode]string{ModeReadOnly: "default", ModeAsk: "default", ModeAuto: "auto_edit", ModeYolo: "yolo"}[t.Mode]
			return append(a, "--approval-mode", mode)
		},
		finalize: func(out []byte, emit func(Event), res *Result) {
			// stdout may carry log lines before the JSON document.
			start := strings.IndexByte(string(out), '{')
			var doc struct {
				Response string `json:"response"`
				Error    *struct {
					Message string `json:"message"`
				} `json:"error"`
				Stats struct {
					Models map[string]struct {
						Tokens struct {
							Prompt     int64 `json:"prompt"`
							Candidates int64 `json:"candidates"`
							Cached     int64 `json:"cached"`
						} `json:"tokens"`
					} `json:"models"`
				} `json:"stats"`
			}
			if start < 0 || json.Unmarshal(out[start:], &doc) != nil {
				res.Final = strings.TrimSpace(string(out))
				emit(Event{Time: time.Now(), Kind: EventText, Text: res.Final})
				return
			}
			if doc.Error != nil {
				res.Status, res.Error = StatusError, doc.Error.Message
			}
			for name, m := range doc.Stats.Models {
				res.Model = name
				res.InputTokens += m.Tokens.Prompt
				res.OutputTokens += m.Tokens.Candidates
				res.CacheRead += m.Tokens.Cached
			}
			res.Final = doc.Response
			emit(Event{Time: time.Now(), Kind: EventText, Text: doc.Response})
		},
	}
}

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

func init() {
	Register(NewAPIBackend())
	Register(NewClaudeBackend())
	Register(NewCodexBackend())
	Register(NewGeminiBackend())
	Register(NewOpencodeBackend())
}
