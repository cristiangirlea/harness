// Package hrnmcp exposes concurrent run, batch and inspection tools over MCP.
package hrnmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/flow"
	"github.com/cristiangirlea/harness/internal/flowview"
	"github.com/cristiangirlea/harness/internal/followup"
	"github.com/cristiangirlea/harness/internal/limits"
	"github.com/cristiangirlea/harness/internal/mcp"
	"github.com/cristiangirlea/harness/internal/owner"
	"github.com/cristiangirlea/harness/internal/purpose"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/tools"
	"github.com/cristiangirlea/harness/internal/ws"
)

const instructions = `hrn runs coding tasks on an agent backend of your choice (its own API loop, or the claude, codex, gemini, opencode or agy CLIs) and records every run.

Use "run" to delegate a task. It blocks until the task finishes and returns the final answer, a summary line and a run id; "show" replays a run, "runs" lists recent ones, "backends" says which backends are usable. Several "run" calls may be in flight at once.

For server-side asynchronous jobs, use "remote_submit" then "remote_status" or "remote_events" with a durable cursor; "remote_cancel" requests cancellation. These tools require HRN_SERVER_URL and HRN_TENANT_TOKEN in the MCP server environment. Remote submission currently supports only read-only API jobs on registered projects.

Use "batch" to fan out: several tasks at once, each on its own branch and git worktree, checked by gate commands, all in one call. "batch_show" reads a batch's results and each task's diff; "batch_merge" merges the passing branches into the current branch.

There is no terminal behind this server. Mode "ask" puts each write and command to you through the host, when the host supports elicitation, and denies it otherwise. Use "readonly" for investigation, "auto" to allow file edits without asking, "yolo" to also allow shell commands.`

// NewServer builds an MCP server using defaultDir for unspecified workspaces.
// It refuses allow_dangerous because only the operator may lift the caller's guard.
func NewServer(defaultDir string) *mcp.Server { return newServer(defaultDir, false) }

// NewDangerousServer allows guarded runs to escalate when explicitly enabled by the operator.
func NewDangerousServer(defaultDir string) *mcp.Server { return newServer(defaultDir, true) }

// errDangerousRefused names no way around it: the model reads it.
var errDangerousRefused = errors.New("allow_dangerous is refused by this hrn MCP server: only the operator can lift the guard")

func newServer(defaultDir string, allowDangerous bool) *mcp.Server {
	s := mcp.NewServer(mcp.Implementation{Name: "hrn", Version: agent.Version}, instructions)
	s.AddTool(runTool(defaultDir, allowDangerous))
	s.AddTool(runsTool())
	s.AddTool(showTool())
	s.AddTool(flowTool())
	s.AddTool(backendsTool())
	s.AddTool(batchTool(defaultDir, allowDangerous))
	s.AddTool(batchShowTool())
	s.AddTool(batchMergeTool())
	s.AddTool(remoteSubmitTool())
	s.AddTool(remoteStatusTool())
	s.AddTool(remoteEventsTool())
	s.AddTool(remoteCancelTool())
	return s
}

func runProperties(backends []any, defaultDir string) map[string]any {
	props := agent.Options{}.Schema()
	mode := props["mode"].(map[string]any)
	mode["description"] = mode["description"].(string) +
		" Default readonly here. ask puts each write and command to you when this host supports elicitation, and denies it otherwise."
	props["prompt"] = str("The task, as you would brief an engineer.")
	props["purpose"] = str("Optional declared purpose: review, debug, implement, refactor, test, research, plan, ops; otherwise a leading Purpose: category header is used.")
	backend := map[string]any{"type": "string", "description": "Default: the backend preference, or api when credentials exist, otherwise claude; an enabled route for the prompt's purpose takes precedence."}
	if len(backends) > 0 { // an empty enum would make every value invalid; the handler reports why
		backend["enum"] = backends
	}
	props["backend"] = backend
	props["dir"] = str("Workspace directory. Default: " + defaultDir)
	props["continue_run_id"] = str("Continue a finished run, in its workspace and mode unless dir or mode say otherwise. " +
		"On the same backend it carries on from that run's conversation (api) or a fork of its session (claude, codex). " +
		"On another backend the earlier turns are handed over as a transcript, so one conversation can move between agents. " +
		"The model is inherited only from runs on the same backend.")
	props["fresh"] = map[string]any{"type": "boolean", "description": "With continue_run_id: start a new session on the backend instead of resuming or forking any, " +
		"handed the earlier run over from its record (its task, status, final answer, changed files and gate results) ahead of prompt. " +
		"For a phase boundary (implementation, review, fixes) where the next step needs a short brief rather than the whole long context."}
	return props
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

func runTool(defaultDir string, allowDangerous bool) mcp.Tool {
	names := agent.Names()
	if p, err := agent.BackendPreferences(runlog.Home()); err == nil {
		names = p.Filter(names)
	} else {
		names = nil // Invalid preferences offer no backends; the handler reports the error.
	}
	enum := make([]any, len(names))
	for i, n := range names {
		enum[i] = n
	}
	props := runProperties(enum, defaultDir)
	if !allowDangerous {
		delete(props, "allow_dangerous") // not offered where it is refused
	}
	return mcp.Tool{
		Name:  "run",
		Title: "Run a task",
		Description: "Run a coding task with an agent backend in a workspace directory and wait for it to finish. " +
			"Optionally declare its purpose; this is recorded without changing the prompt. " +
			"Returns the final answer, a summary line (status, tokens, cost, duration) and the run id. " +
			"A run that reaches its usage limit is carried on by a ready enabled partner (claude and codex pair by default), " +
			"which then sees the workspace, unless no_switch is set.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": props,
			"required":   []any{"prompt"},
		},
		Annotations: &mcp.Annotations{ReadOnlyHint: mcp.Bool(false), OpenWorldHint: mcp.Bool(true)},
		Handler: func(ctx context.Context, call mcp.Call) (mcp.CallResult, error) {
			args, err := decode[struct {
				Prompt        string `json:"prompt"`
				Purpose       string `json:"purpose,omitempty"`
				Backend       string `json:"backend"`
				Dir           string `json:"dir"`
				ContinueRunID string `json:"continue_run_id"`
				Fresh         bool   `json:"fresh"`
				agent.Options
			}](call.Arguments)
			if err != nil {
				return mcp.CallResult{}, err
			}
			if strings.TrimSpace(args.Prompt) == "" {
				return mcp.CallResult{}, errors.New("prompt is required")
			}
			if args.Purpose != "" && !purpose.Valid(args.Purpose) {
				return mcp.CallResult{}, fmt.Errorf("purpose %q: one of %s", args.Purpose, strings.Join(purpose.Declarable(), ", "))
			}
			if args.Purpose == "" {
				args.Purpose = purpose.Declared(args.Prompt)
			}
			if args.Fresh && args.ContinueRunID == "" {
				return mcp.CallResult{}, errors.New("fresh needs continue_run_id: it starts a new session handed an earlier run over")
			}
			if args.AllowDangerous && !allowDangerous {
				return mcp.CallResult{}, errDangerousRefused
			}
			modeGiven := args.Mode != ""
			if !modeGiven {
				args.Mode = agent.ModeReadOnly
			}
			if err := args.Options.Validate(); err != nil {
				return mcp.CallResult{}, err
			}
			preference, err := agent.BackendPreferences(runlog.Home())
			if err != nil {
				return text("run: "+err.Error(), nil, true), nil
			}
			if err := preference.Check(args.Backend); err != nil {
				return text("run: "+err.Error(), nil, true), nil
			}
			// Mirrors hrn run: limit routing first, then the earlier run's mode and purpose unless named.
			var routed string
			if !args.NoSwitch && args.ContinueRunID != "" {
				meant := args.Backend
				if args.Backend, routed = limits.Choose(args.Backend, args.ContinueRunID); args.Backend != meant {
					args.Model = ""
				}
			}
			var plan followup.Plan
			if args.ContinueRunID != "" {
				plan, err = followup.Make(followup.Request{RunID: args.ContinueRunID, Backend: args.Backend, Dir: args.Dir, Model: args.Model, Retrieval: args.Retrieval, Fresh: args.Fresh})
				if err != nil {
					return mcp.CallResult{}, err
				}
				defer plan.Release()
				args.Backend, args.Dir, args.Model = plan.Backend, plan.Dir, plan.Model
				if !modeGiven && plan.Prev.Task.Mode != "" {
					args.Mode = plan.Prev.Task.Mode
				}
				if args.Purpose == "" {
					args.Purpose = purpose.Inherited(plan.Prev.Task.Purpose, plan.Prev.Task.Prompt)
				}
			}
			// Inherited workspaces require the same validation as explicit ones.
			dir := args.Dir
			if dir == "" {
				dir = defaultDir
			}
			ws, err := tools.NewWorkspace(dir)
			if err != nil {
				return mcp.CallResult{}, fmt.Errorf("dir %s: %w", dir, err)
			}
			dir = ws.Root
			routedBy, routeNote, err := args.Options.RouteWithNote(args.Purpose, args.Prompt, &args.Backend, runlog.Home())
			if err != nil {
				return mcp.CallResult{}, err
			}
			if args.ContinueRunID != "" {
				routeNote = agent.ContinuationNote(args.ContinueRunID, routed)
			}
			if !args.NoSwitch && args.ContinueRunID == "" {
				meant := args.Backend
				if args.Backend, routed = limits.Choose(args.Backend, ""); args.Backend != meant {
					if routedBy != nil {
						args.Options.Effort = "" // the row measured the limited backend, not its partner
					}
					args.Model, routedBy, routeNote = "", nil, routed
				}
			}
			if args.Backend == "" {
				args.Backend = agent.DefaultBackendNameIn(runlog.Home())
				if routedBy == nil {
					routeNote = agent.WithDefault(routeNote, args.Backend)
				}
			}
			if err := preference.Check(args.Backend); err != nil {
				return text("run: "+err.Error(), nil, true), nil
			}
			b, err := agent.Get(args.Backend)
			if err != nil {
				return mcp.CallResult{}, err
			}
			task := agent.Task{Prompt: args.Prompt, Purpose: args.Purpose, Dir: dir, Options: args.Options}
			turnDefault := task.ApplyTurnDefault(args.Backend)
			if task.MCP, err = mcp.ConfigPaths(args.MCP); err != nil {
				return mcp.CallResult{}, err
			}
			rec, err := runlog.CreateRouted(args.Backend, task, args.ContinueRunID, plan.Route(), routedBy, routeNote)
			if err != nil {
				return mcp.CallResult{}, fmt.Errorf("creating run record: %w", err)
			}
			task.RunDir = rec.Dir()
			issue, err := takeRunIssue(ctx, args.Options, dir, rec.ID(), args.Backend)
			if err != nil {
				_ = rec.Close(agent.Result{Status: agent.StatusError, Error: err.Error()})
				return text("run: "+err.Error(), map[string]any{"run_id": rec.ID()}, true), nil
			}
			issueReason := "run " + rec.ID() + " ended"
			defer issue.Release(ctx, issueReason)
			if args.Mode == agent.ModeAsk && call.ClientSupports("elicitation") {
				// Named by run, because two runs may be asking at once.
				task.Approve = approver(ctx, call, rec.ID())
			}
			emit := func(e agent.Event) {
				rec.Event(e)
				switch e.Kind {
				case agent.EventToolCall:
					call.Progress(printableFlow(strings.TrimSpace("→ " + tools.OneLine(e.Tool, 80) + " " + tools.OneLine(e.Text, 120))))
				case agent.EventError:
					call.Progress("error: " + printableNote(e.Text, 200))
				}
			}
			var notes []string
			if turnDefault {
				emit(agent.Event{Time: time.Now(), Kind: agent.EventInfo, Text: agent.TurnDefaultInfo})
				call.Progress(agent.TurnDefaultInfo)
				notes = append(notes, agent.TurnDefaultInfo)
			}
			if issue != nil {
				notes = append(notes, issue.Notes...)
			}
			if warning := plan.ContextWarning(); warning != "" {
				call.Progress(warning)
				notes = append(notes, warning)
			}
			if routed != "" {
				emit(agent.Event{Time: time.Now(), Kind: agent.EventInfo, Text: routed})
				notes = append(notes, routed)
			}
			plan.Apply(&task, rec.Dir(), emit)
			res := agent.Run(ctx, b, task, emit)
			recordErr := rec.Close(res)
			recordMessage := ""
			if recordErr != nil {
				recordMessage = runlog.CloseMessage(recordErr)
				call.Progress(recordMessage)
			}
			structured := map[string]any{"run_id": rec.ID(), "result": res}

			// Switch only once to prevent limited partners from handing work back indefinitely.
			var next *limits.CarryOn
			var why string
			if ctx.Err() == nil {
				next, why, err = limits.After(rec.ID(), args.Backend, task, res)
			} else if err := limits.Note(args.Backend, res); err != nil {
				notes = append(notes, "the limit was not remembered: "+err.Error())
			}
			switch {
			case err != nil:
				notes = append(notes, err.Error())
			case next == nil && why != "":
				notes = append(notes, why)
			case next != nil:
				defer next.Plan.Release()
				if turnDefault {
					next.Task.MaxTurns = 0
				}
				if next.Task.Purpose == "" {
					next.Task.Purpose = purpose.Inherited(task.Purpose, args.Prompt)
				}
				nextDefault := next.Task.ApplyTurnDefault(next.To)
				nb, err := agent.Get(next.To)
				if err != nil {
					return mcp.CallResult{}, err
				}
				nrec, err := runlog.CreateContinuing(next.To, next.Task, rec.ID())
				if err != nil {
					return mcp.CallResult{}, fmt.Errorf("creating run record: %w", err)
				}
				next.Task.RunDir = nrec.Dir()
				if next.Task.Approve != nil {
					next.Task.Approve = approver(ctx, call, nrec.ID())
				}
				limitedID := rec.ID()
				rec = nrec // emit writes to the new record now
				call.Progress(next.Why)
				emit(agent.Event{Time: time.Now(), Kind: agent.EventInfo, Text: next.Why})
				if nextDefault {
					emit(agent.Event{Time: time.Now(), Kind: agent.EventInfo, Text: agent.TurnDefaultInfo})
					call.Progress(agent.TurnDefaultInfo)
					notes = append(notes, agent.TurnDefaultInfo)
				}
				next.Plan.Apply(&next.Task, nrec.Dir(), emit)
				res = agent.Run(ctx, nb, next.Task, emit)
				if recordErr != nil {
					notes = append(notes, "the limited run "+limitedID+": "+recordMessage)
				}
				recordErr, recordMessage = nrec.Close(res), ""
				if recordErr != nil {
					recordMessage = runlog.CloseMessage(recordErr)
					call.Progress(recordMessage)
				}
				notes = append(notes, next.Why)
				// Limited too: remembered, so the next run does not try it first.
				if err := limits.Note(next.To, res); err != nil {
					notes = append(notes, "the limit was not remembered: "+err.Error())
				}
				structured = map[string]any{"run_id": nrec.ID(), "result": res, "limited_run_id": limitedID}
			}

			if issue != nil {
				seen := len(issue.Notes)
				if err := issue.Release(ctx, issueReason); err != nil {
					notes = append(notes, err.Error())
				}
				notes = append(notes, issue.Notes[seen:]...)
			}
			res.Error, res.Final = printableFlow(res.Error), printableFlow(res.Final)
			structured["result"] = res
			body := res.Final
			if res.Status != agent.StatusOK {
				body = fmt.Sprintf("status: %s\n%s", res.Status, res.Error)
				if res.Final != "" {
					body += "\n\n" + res.Final
				}
			}
			if len(notes) > 0 {
				body = strings.Join(notes, "\n") + "\n\n" + body
			}
			if recordErr != nil {
				body = recordMessage + "\n\n" + body
				structured["record_error"] = recordErr.Error()
			}
			// Lost events leave an ok run ok: a host may retry a tool error and spend the
			// plan again. A result.json that was not saved leaves the run looking unfinished,
			// so that one is an error.
			return text(body+"\n\n"+summary(rec.ID(), res),
				structured, res.Status != agent.StatusOK || runlog.ResultNotSaved(recordErr)), nil
		},
	}
}

// takeRunIssue leases the run's issue before its agent starts; nil when it names none.
func takeRunIssue(ctx context.Context, opts agent.Options, dir, run, backend string) (*ws.IssueLease, error) {
	if opts.Issue == "" {
		return nil, nil
	}
	return ws.TakeRunIssue(ctx, runlog.Home(), opts.Issue, dir, ws.Lease{Holder: "hrn mcp run", Run: run, Backend: backend}, opts.TakeOver)
}

func runsTool() mcp.Tool {
	return mcp.Tool{
		Name:        "runs",
		Title:       "List recent runs",
		Description: "List recent hrn runs, newest first: id, backend, status, duration and prompt.",
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
					rec.Result.Duration.Round(time.Second), tools.OneLine(rec.Task.Prompt, 70))
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
			rec.Result.Error, rec.Result.Final = printableFlow(rec.Result.Error), printableFlow(rec.Result.Final)
			var b strings.Builder
			fmt.Fprintf(&b, "%s\n%s · %s · mode %s%s\n\n", tools.OneLine(rec.Task.Prompt, 200), rec.Backend, rec.Task.Dir, rec.Task.Mode, continues(rec))
			if rec.RecordError != "" {
				fmt.Fprintf(&b, "run record incomplete: %s\n\n", rec.RecordError)
			}
			var events []agent.Event
			if args.Events == nil || *args.Events {
				if events, err = runlog.Events(args.RunID); err != nil {
					return mcp.CallResult{}, err
				}
				for i := range events {
					events[i].Text = printableFlow(events[i].Text)
					events[i].Tool = printableFlow(events[i].Tool)
					events[i].Input = printableInput(events[i].Input)
					e := events[i]
					switch e.Kind {
					case agent.EventText:
						b.WriteString(strings.TrimRight(e.Text, "\n") + "\n")
					case agent.EventToolCall:
						fmt.Fprintf(&b, "→ %s %s\n", e.Tool, tools.OneLine(e.Text, 160))
					case agent.EventToolResult:
						mark := "✓"
						if e.IsError {
							mark = "✗"
						}
						fmt.Fprintf(&b, "  %s %s\n", mark, tools.OneLine(e.Text, 140))
					case agent.EventInfo:
						fmt.Fprintf(&b, "· %s\n", tools.OneLine(e.Text, 200))
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

func flowTool() mcp.Tool {
	return mcp.Tool{
		Name: "flow", Title: "Show an execution flow",
		Description: "Read a run or batch execution tree, including recorded reasons and next steps.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":    str("A run or batch id."),
				"level": map[string]any{"type": "string", "enum": []any{"simple", "technical", "debug"}, "description": "Detail level (default technical)."},
			},
			"required": []any{"id"},
		},
		Annotations: &mcp.Annotations{ReadOnlyHint: mcp.Bool(true)},
		Handler: func(ctx context.Context, call mcp.Call) (mcp.CallResult, error) {
			args, err := decode[struct {
				ID    string `json:"id"`
				Level string `json:"level"`
			}](call.Arguments)
			if err != nil {
				return mcp.CallResult{}, err
			}
			if args.ID == "" || strings.ContainsAny(args.ID, `/\\`) {
				return text("no run or batch "+args.ID, nil, true), nil
			}
			if args.Level == "" {
				args.Level = "technical"
			}
			if args.Level != "simple" && args.Level != "technical" && args.Level != "debug" {
				return mcp.CallResult{}, fmt.Errorf("invalid flow level %q: use simple, technical or debug", args.Level)
			}
			view, err := flowview.Load(owner.LocalViewer(), args.ID)
			if err != nil {
				if errors.Is(err, flowview.ErrNotFound) {
					return text("no run or batch "+args.ID, nil, true), nil
				}
				return mcp.CallResult{}, err
			}
			tree := flowview.Filter(view.Tree, args.Level)
			var b strings.Builder
			if err := flowview.WriteText(&b, view.Summary, view.Issues, tree, args.Level); err != nil {
				return mcp.CallResult{}, err
			}
			body, cut := truncateFlowText(printableFlow(b.String()), 64*1024)
			structured := map[string]any{"version": view.Version, "summary": printableFlow(view.Summary), "tree": cleanFlowTree(tree)}
			if len(view.Issues) > 0 {
				structured["issues"] = cleanIssues(view.Issues)
			}
			if cut {
				body += "\n[flow text truncated at 64 KiB]\n"
				// The tree is at least as large as the text; a lower level or hrn flow --json gives it whole.
				structured["tree"], structured["truncated"] = []*flow.Node{}, true
			}
			return text(body, structured, false), nil
		},
	}
}

func cleanFlowTree(nodes []*flow.Node) []*flow.Node {
	out := make([]*flow.Node, len(nodes))
	for i, n := range nodes {
		copy := *n
		copy.Name = printableFlow(flow.Redact(copy.Name))
		copy.Text = printableFlow(flow.Redact(copy.Text))
		copy.Reason = printableFlow(flow.Redact(copy.Reason))
		copy.Next = printableFlow(flow.Redact(copy.Next))
		copy.Step, copy.Run, copy.Task = printableFlow(copy.Step), printableFlow(copy.Run), printableFlow(copy.Task)
		copy.Status = flow.Status(printableFlow(string(copy.Status)))
		copy.Children = cleanFlowTree(copy.Children)
		out[i] = &copy
	}
	return out
}

func cleanIssues(issues []ws.IssueStatus) []ws.IssueStatus {
	out := make([]ws.IssueStatus, len(issues))
	for i, s := range issues {
		for _, f := range []*string{&s.Issue, &s.State, &s.Why, &s.Holder, &s.Run, &s.Backend, &s.Branch, &s.PR} {
			*f = printableFlow(*f)
		}
		out[i] = s
	}
	return out
}

// Decoded notes may be printed directly to a terminal by MCP clients.
func printableNote(s string, max int) string {
	return printableFlow(tools.OneLine(s, max))
}

func printableInput(input json.RawMessage) json.RawMessage {
	if len(input) == 0 {
		return input
	}
	var value any
	dec := json.NewDecoder(strings.NewReader(string(input)))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil || !json.Valid(input) {
		value = string(input)
	}
	data, _ := json.Marshal(printableJSONValue(value))
	return data
}

func printableJSONValue(value any) any {
	switch v := value.(type) {
	case string:
		return printableFlow(v)
	case []any:
		for i := range v {
			v[i] = printableJSONValue(v[i])
		}
	case map[string]any:
		out := make(map[string]any, len(v))
		for _, key := range slices.Sorted(maps.Keys(v)) {
			// Keys that differ only in filtered characters must not merge and hide an argument.
			k := printableFlow(key)
			for _, taken := out[k]; taken; _, taken = out[k] {
				k += "�"
			}
			out[k] = printableJSONValue(v[key])
		}
		return out
	}
	return value
}

func printableFlow(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return '�'
		}
		return r
	}, s)
}

func truncateFlowText(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	s = s[:limit]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s, true
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
			p, err := agent.BackendPreferences(runlog.Home())
			if err != nil {
				return text(err.Error(), nil, true), nil
			}
			def := agent.DefaultBackendNameIn(runlog.Home())
			now := time.Now()
			for _, name := range agent.Names() {
				state := "disabled"
				if p.Enabled(name) {
					state = "enabled"
				}
				if name == def {
					state += " (default)"
				}
				be, _ := agent.Get(name)
				src, err := be.Detect()
				var entry map[string]any
				if err != nil {
					fmt.Fprintf(&b, "✗ %-9s %s: %s\n", name, state, err)
					entry = map[string]any{"name": name, "available": false, "detail": err.Error()}
				} else {
					fmt.Fprintf(&b, "✓ %-9s %s: %s\n", name, state, src)
					entry = map[string]any{"name": name, "available": true, "detail": src}
				}
				if s := limits.Status(name, now); s != "" {
					fmt.Fprintf(&b, "  %-9s %s\n", "", s)
					entry["limited_until"] = limits.Until(name, now)
				}
				entry["enabled"] = p.Enabled(name)
				entry["default"] = name == def
				list = append(list, entry)
			}
			return text(b.String(), map[string]any{"backends": list, "default": def}, false), nil
		},
	}
}

// Serialize prompts to prevent interleaving and deny anything other than a clear yes.
func approver(ctx context.Context, call mcp.Call, runID string) func(agent.ApprovalRequest) bool {
	var asking sync.Mutex
	return func(req agent.ApprovalRequest) bool {
		asking.Lock()
		defer asking.Unlock()
		actx, cancel := context.WithTimeout(ctx, approvalTimeout)
		defer cancel()

		var answer struct {
			Action  string `json:"action"`
			Content struct {
				Approve bool `json:"approve"`
			} `json:"content"`
		}
		params := map[string]any{
			// Truncate keeps the summary's head and tail; never cut it further, as it is what is approved.
			"message": fmt.Sprintf("hrn run %s wants to run %s:\n%s", runID, req.Tool, tools.Truncate(req.Summary)),
			"requestedSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"approve": map[string]any{"type": "boolean", "title": "Allow"}},
				"required":   []any{"approve"},
			},
		}
		if err := call.Request(actx, "elicitation/create", params, &answer); err != nil {
			call.Progress("approval request failed, denying: " + err.Error())
			return false
		}
		allowed := answer.Action == "accept" && answer.Content.Approve
		decision := "denied"
		if allowed {
			decision = "allowed"
		}
		call.Progress(fmt.Sprintf("%s %s: %s", decision, req.Tool, tools.OneLine(req.Summary, 120)))
		return allowed
	}
}

// Unanswered approvals must not hold a run open indefinitely.
const approvalTimeout = 5 * time.Minute

func continues(rec runlog.Record) string {
	s := ""
	if rec.Continues != "" {
		s += " · continues " + rec.Continues
	}
	if rec.Result.Session != "" {
		s += " · session " + rec.Result.Session
	}
	return s
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
	missing := ""
	if res.UsageMissing() {
		missing = ", usage missing"
	}
	return fmt.Sprintf("── %s · %s · %s · %d turns · %d in / %d out (cache %d read%s)%s · %s ── run %s",
		res.Status, res.Backend, res.Model, res.Turns, res.InputTokens, res.OutputTokens, res.CacheRead, missing,
		cost, res.Duration.Round(100*time.Millisecond), runID)
}
