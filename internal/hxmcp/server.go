// Package hxmcp exposes hx itself as an MCP server, so a Claude Code session
// (or any MCP host) can delegate tasks to any backend, several at a time, and
// read the run records back. Requests are served concurrently, so parallel
// tool calls from the host become parallel agents.
package hxmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/mcp"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/tools"
)

const instructions = `hx runs coding tasks on an agent backend of your choice (its own API loop, or the claude, codex, gemini or opencode CLIs) and records every run.

Use "run" to delegate a task. It blocks until the task finishes and returns the final answer, a summary line and a run id; "show" replays a run, "runs" lists recent ones, "backends" says which backends are usable. Several "run" calls may be in flight at once.

There is no terminal behind this server, so mode "ask" denies every write and command. Use "readonly" for investigation, "auto" to allow file edits, "yolo" to also allow shell commands.`

// NewServer builds the hx MCP server. defaultDir is the workspace used when
// a run does not name one, normally the host's project directory.
func NewServer(defaultDir string) *mcp.Server {
	s := mcp.NewServer(mcp.Implementation{Name: "hx", Version: agent.Version}, instructions)
	s.AddTool(runTool(defaultDir))
	s.AddTool(runsTool())
	s.AddTool(showTool())
	s.AddTool(backendsTool())
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }

func decode[T any](raw json.RawMessage) (T, error) {
	var v T
	if len(raw) == 0 {
		return v, nil
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("invalid arguments: %w", err)
	}
	return v, nil
}

func text(s string, structured any, isError bool) mcp.CallResult {
	return mcp.CallResult{Content: []mcp.Content{mcp.TextContent(tools.Truncate(s))}, StructuredContent: structured, IsError: isError}
}

func runTool(defaultDir string) mcp.Tool {
	names := agent.Names()
	enum := make([]any, len(names))
	for i, n := range names {
		enum[i] = n
	}
	return mcp.Tool{
		Name:  "run",
		Title: "Run a task",
		Description: "Run a coding task with an agent backend in a workspace directory and wait for it to finish. " +
			"Returns the final answer, a summary line (status, tokens, cost, duration) and the run id.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompt":  str("The task, as you would brief an engineer."),
				"backend": map[string]any{"type": "string", "enum": enum, "description": "Default: api when API credentials exist, otherwise claude."},
				"model":   str("Model id for the backend. Optional."),
				"dir":     str("Workspace directory. Default: " + defaultDir),
				"mode": map[string]any{"type": "string", "enum": []any{"readonly", "ask", "auto", "yolo"},
					"description": "Permission mode, default readonly. ask denies all writes here (no terminal); auto allows file edits; yolo also allows commands."},
				"max_turns": num("Stop after this many model turns."),
				"effort":    str("api backend only: low | medium | high | xhigh | max."),
				"mcp": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"description": "MCP config files (Claude Code .mcp.json format) whose stdio servers the task may use as tools."},
			},
			"required": []any{"prompt"},
		},
		Annotations: &mcp.Annotations{ReadOnlyHint: mcp.Bool(false), OpenWorldHint: mcp.Bool(true)},
		Handler: func(ctx context.Context, call mcp.Call) (mcp.CallResult, error) {
			args, err := decode[struct {
				Prompt   string   `json:"prompt"`
				Backend  string   `json:"backend"`
				Model    string   `json:"model"`
				Dir      string   `json:"dir"`
				Mode     string   `json:"mode"`
				MaxTurns int      `json:"max_turns"`
				Effort   string   `json:"effort"`
				MCP      []string `json:"mcp"`
			}](call.Arguments)
			if err != nil {
				return mcp.CallResult{}, err
			}
			if strings.TrimSpace(args.Prompt) == "" {
				return mcp.CallResult{}, errors.New("prompt is required")
			}
			if args.Mode == "" {
				args.Mode = string(agent.ModeReadOnly)
			}
			mode, err := agent.ParseMode(args.Mode)
			if err != nil {
				return mcp.CallResult{}, err
			}
			dir := args.Dir
			if dir == "" {
				dir = defaultDir
			}
			if dir, err = filepath.Abs(dir); err != nil {
				return mcp.CallResult{}, err
			}
			if info, err := os.Stat(dir); err != nil || !info.IsDir() {
				return mcp.CallResult{}, fmt.Errorf("dir %s is not a directory", dir)
			}
			if args.Backend == "" {
				args.Backend = agent.DefaultBackendName()
			}
			b, err := agent.Get(args.Backend)
			if err != nil {
				return mcp.CallResult{}, err
			}
			task := agent.Task{Prompt: args.Prompt, Dir: dir, Model: args.Model, Mode: mode, MaxTurns: args.MaxTurns, Effort: args.Effort}
			if task.MCP, err = mcp.ConfigPaths(args.MCP); err != nil {
				return mcp.CallResult{}, err
			}
			rec, err := runlog.Create(args.Backend, task)
			if err != nil {
				return mcp.CallResult{}, fmt.Errorf("creating run record: %w", err)
			}
			emit := func(e agent.Event) {
				rec.Event(e)
				switch e.Kind {
				case agent.EventToolCall:
					call.Progress(strings.TrimSpace("→ " + e.Tool + " " + oneLine(e.Text, 120)))
				case agent.EventError:
					call.Progress("error: " + oneLine(e.Text, 200))
				}
			}
			res := b.Run(ctx, task, emit)
			if err := rec.Close(res); err != nil {
				call.Progress("saving run: " + err.Error())
			}
			body := res.Final
			if res.Status != agent.StatusOK {
				body = fmt.Sprintf("status: %s\n%s", res.Status, res.Error)
				if res.Final != "" {
					body += "\n\n" + res.Final
				}
			}
			return text(body+"\n\n"+summary(rec.ID(), res),
				map[string]any{"run_id": rec.ID(), "result": res}, res.Status != agent.StatusOK), nil
		},
	}
}

func runsTool() mcp.Tool {
	return mcp.Tool{
		Name:        "runs",
		Title:       "List recent runs",
		Description: "List recent hx runs, newest first: id, backend, status, duration and prompt.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"limit": num("Maximum runs to return (default 20).")}},
		Annotations: &mcp.Annotations{ReadOnlyHint: mcp.Bool(true)},
		Handler: func(ctx context.Context, call mcp.Call) (mcp.CallResult, error) {
			args, err := decode[struct {
				Limit int `json:"limit"`
			}](call.Arguments)
			if err != nil {
				return mcp.CallResult{}, err
			}
			if args.Limit <= 0 {
				args.Limit = 20
			}
			recs, err := runlog.List(args.Limit)
			if err != nil {
				return mcp.CallResult{}, err
			}
			if len(recs) == 0 {
				return text("no runs yet", map[string]any{"runs": []any{}}, false), nil
			}
			var b strings.Builder
			var list []map[string]any
			for _, rec := range recs {
				status := string(rec.Result.Status)
				if !rec.Done {
					status = "running?"
				}
				fmt.Fprintf(&b, "%s  %-8s %-9s %7s  %s\n", rec.ID, rec.Backend, status,
					rec.Result.Duration.Round(time.Second), oneLine(rec.Task.Prompt, 70))
				list = append(list, map[string]any{"id": rec.ID, "backend": rec.Backend, "status": status,
					"started": rec.Started, "duration_ns": rec.Result.Duration, "prompt": rec.Task.Prompt, "dir": rec.Task.Dir})
			}
			return text(b.String(), map[string]any{"runs": list}, false), nil
		},
	}
}

func showTool() mcp.Tool {
	return mcp.Tool{
		Name:        "show",
		Title:       "Show a run",
		Description: "Replay a recorded run: its task, the events (tool calls, results, errors) and the result.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"run_id": str("The run id (from run or runs)."),
				"events": map[string]any{"type": "boolean", "description": "Include the event transcript (default true)."},
			},
			"required": []any{"run_id"},
		},
		Annotations: &mcp.Annotations{ReadOnlyHint: mcp.Bool(true)},
		Handler: func(ctx context.Context, call mcp.Call) (mcp.CallResult, error) {
			args, err := decode[struct {
				RunID  string `json:"run_id"`
				Events *bool  `json:"events"`
			}](call.Arguments)
			if err != nil {
				return mcp.CallResult{}, err
			}
			if args.RunID == "" || strings.ContainsAny(args.RunID, `/\`) {
				return mcp.CallResult{}, errors.New("run_id is required")
			}
			rec, err := runlog.Load(args.RunID)
			if err != nil {
				return mcp.CallResult{}, fmt.Errorf("run %s: %w", args.RunID, err)
			}
			var b strings.Builder
			fmt.Fprintf(&b, "%s\n%s · %s · mode %s\n\n", oneLine(rec.Task.Prompt, 200), rec.Backend, rec.Task.Dir, rec.Task.Mode)
			var events []agent.Event
			if args.Events == nil || *args.Events {
				if events, err = runlog.Events(args.RunID); err != nil {
					return mcp.CallResult{}, err
				}
				for _, e := range events {
					switch e.Kind {
					case agent.EventText:
						b.WriteString(strings.TrimRight(e.Text, "\n") + "\n")
					case agent.EventToolCall:
						fmt.Fprintf(&b, "→ %s %s\n", e.Tool, oneLine(e.Text, 160))
					case agent.EventToolResult:
						mark := "✓"
						if e.IsError {
							mark = "✗"
						}
						fmt.Fprintf(&b, "  %s %s\n", mark, oneLine(e.Text, 140))
					case agent.EventInfo:
						fmt.Fprintf(&b, "· %s\n", oneLine(e.Text, 200))
					case agent.EventError:
						fmt.Fprintf(&b, "error: %s\n", e.Text)
					}
				}
				b.WriteString("\n")
			}
			if rec.Done {
				b.WriteString(summary(rec.ID, rec.Result))
			} else {
				b.WriteString("(no result yet)")
			}
			return text(b.String(), map[string]any{"record": rec, "events": events}, false), nil
		},
	}
}

func backendsTool() mcp.Tool {
	return mcp.Tool{
		Name:        "backends",
		Title:       "List backends",
		Description: "Show which agent backends are usable on this machine and where they were found.",
		Annotations: &mcp.Annotations{ReadOnlyHint: mcp.Bool(true)},
		Handler: func(ctx context.Context, call mcp.Call) (mcp.CallResult, error) {
			var b strings.Builder
			var list []map[string]any
			for _, name := range agent.Names() {
				be, _ := agent.Get(name)
				src, err := be.Detect()
				if err != nil {
					fmt.Fprintf(&b, "✗ %-9s %s\n", name, err)
					list = append(list, map[string]any{"name": name, "available": false, "detail": err.Error()})
				} else {
					fmt.Fprintf(&b, "✓ %-9s %s\n", name, src)
					list = append(list, map[string]any{"name": name, "available": true, "detail": src})
				}
			}
			return text(b.String(), map[string]any{"backends": list, "default": agent.DefaultBackendName()}, false), nil
		},
	}
}

func summary(runID string, res agent.Result) string {
	cost := ""
	if res.CostUSD > 0 {
		approx := ""
		if res.CostEstimate {
			approx = "~"
		}
		cost = fmt.Sprintf(" · %s$%.4f", approx, res.CostUSD)
	}
	return fmt.Sprintf("── %s · %s · %s · %d turns · %d in / %d out (cache %d read)%s · %s ── run %s",
		res.Status, res.Backend, res.Model, res.Turns, res.InputTokens, res.OutputTokens, res.CacheRead,
		cost, res.Duration.Round(100*time.Millisecond), runID)
}

func oneLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > n {
		s = string(r[:n]) + "…"
	}
	return s
}
