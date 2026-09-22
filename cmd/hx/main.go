// Command hx is a personal agent harness: one CLI that runs a coding task on
// the harness's own API agent loop or on an installed agent CLI, and records
// every run the same way.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/hxmcp"
	"github.com/cristiangirlea/harness/internal/mcp"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/tools"
)

const usage = `hx — personal agent harness

Usage:
  hx run [flags] <prompt...>     run a task (prompt from args, or stdin when omitted or "-")
  hx backends                    show which backends are usable
  hx runs [-n N]                 list recent runs
  hx show [--json] <run-id>      replay a recorded run
  hx mcp [--dir PATH]            serve hx as an MCP server on stdio; --dir is the default workspace (default: current directory)
  hx batch [flags] <tasks.json>  run tasks in parallel, each in its own git worktree, with gates and retries
  hx batch rm <batch-id>         remove the worktrees and branches a batch created
  hx report [flags]              tables over the finished runs and the batches: pass rate, duration, turns, cost

Run flags:
  -b, --backend NAME   api | claude | codex | gemini | opencode (default: api if credentials exist, else claude)
  -m, --model NAME     model id (api default: claude-opus-5)
  -C, --dir PATH       workspace directory (default: current directory)
      --mode MODE      readonly | ask | auto | yolo (default: ask)
      --max-turns N    stop after N model turns (default 50 for api)
      --effort LEVEL   low | medium | high | xhigh | max (api backend)
      --json           print events as JSON lines instead of formatted output
      --thinking       show model thinking summaries
      --mcp FILE       MCP config (Claude Code .mcp.json format); repeatable. ~/.hx/mcp.json is loaded when present
      --no-instructions
                       skip the project instruction files (api backend)
      --no-context-edits
                       keep every tool result in the request instead of letting the server clear old ones (api backend)
      --timeout D      stop the run after D (e.g. 20m)
      --max-cost USD   stop the api loop before a turn that would pass this estimated cost
      --task-budget N  token ceiling the api backend gives the model to pace itself (min 20000)
      --allow-dangerous
                       let run_command run git push, sudo, rm -rf / and the like (api backend)

Batch flags:
      --parallel N     agents at once (default: the file's value, else 2)
      --allow-dirty    start even though the repository has uncommitted changes
      --json           print the batch record as JSON (progress goes to stderr)
  -v, --verbose        show each agent's tool calls

Report flags:
      --since WINDOW   only runs and batches started within the window, e.g. 7d, 36h, 90m (default: all)
      --by KEY         group runs by backend (default), model or dir
      --json           print the tables as JSON

Runs are recorded under ~/.hx/runs (override with HX_HOME).
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	code := 0
	switch os.Args[1] {
	case "run":
		code, err = cmdRun(os.Args[2:])
	case "backends":
		err = cmdBackends()
	case "runs":
		err = cmdRuns(os.Args[2:])
	case "show":
		err = cmdShow(os.Args[2:])
	case "mcp":
		err = cmdMCP(os.Args[2:])
	case "batch":
		code, err = cmdBatch(os.Args[2:])
	case "report":
		err = cmdReport(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hx:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func cmdRun(args []string) (int, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var backend, model, dir, mode, effort string
	var maxTurns int
	var jsonOut, thinking bool
	for _, name := range []string{"b", "backend"} {
		fs.StringVar(&backend, name, "", "")
	}
	for _, name := range []string{"m", "model"} {
		fs.StringVar(&model, name, "", "")
	}
	for _, name := range []string{"C", "dir"} {
		fs.StringVar(&dir, name, ".", "")
	}
	fs.StringVar(&mode, "mode", "ask", "")
	fs.StringVar(&effort, "effort", "", "")
	fs.IntVar(&maxTurns, "max-turns", 0, "")
	fs.BoolVar(&jsonOut, "json", false, "")
	fs.BoolVar(&thinking, "thinking", false, "")
	noInstructions := fs.Bool("no-instructions", false, "")
	noContextEdits := fs.Bool("no-context-edits", false, "")
	allowDangerous := fs.Bool("allow-dangerous", false, "")
	timeout := fs.Duration("timeout", 0, "")
	maxCost := fs.Float64("max-cost", 0, "")
	taskBudget := fs.Int("task-budget", 0, "")
	var mcpFiles multiFlag
	fs.Var(&mcpFiles, "mcp", "")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return 2, nil
	}

	m, err := agent.ParseMode(mode)
	if err != nil {
		return 2, err
	}
	ws, err := tools.NewWorkspace(dir)
	if err != nil {
		return 2, fmt.Errorf("-C %s: %w", dir, err)
	}
	absDir := ws.Root

	promptFromStdin := fs.NArg() == 0 || (fs.NArg() == 1 && fs.Arg(0) == "-")
	var prompt string
	if promptFromStdin {
		if isTerminal(os.Stdin) {
			fmt.Fprintln(os.Stderr, "Enter the task, then Ctrl+Z Enter (Windows) or Ctrl+D:")
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return 1, err
		}
		prompt = string(data)
	} else {
		prompt = strings.Join(fs.Args(), " ")
	}
	if strings.TrimSpace(prompt) == "" {
		return 2, errors.New("empty prompt")
	}

	if backend == "" {
		backend = agent.DefaultBackendName()
	}
	b, err := agent.Get(backend)
	if err != nil {
		return 2, err
	}

	task := agent.Task{Prompt: prompt, Dir: absDir, Model: model, Mode: m, MaxTurns: maxTurns, Effort: effort, NoInstructions: *noInstructions, NoContextEdits: *noContextEdits, AllowDangerous: *allowDangerous,
		Timeout: *timeout, MaxCostUSD: *maxCost, TaskBudget: *taskBudget}
	if err := agent.ValidateLimits(task.Timeout, task.MaxCostUSD, task.TaskBudget); err != nil {
		return 2, err
	}
	if task.MCP, err = mcp.ConfigPaths(mcpFiles); err != nil {
		return 2, err
	}
	stdin := bufio.NewReader(os.Stdin)
	if !promptFromStdin && isTerminal(os.Stdin) {
		task.Approve = func(req agent.ApprovalRequest) bool {
			fmt.Fprintf(os.Stderr, "\n\x1b[33mallow %s?\x1b[0m %s [y/N] ", req.Tool, tools.OneLine(req.Summary, 300))
			line, _ := stdin.ReadString('\n')
			return strings.EqualFold(strings.TrimSpace(line), "y")
		}
	}

	rec, err := runlog.Create(backend, task)
	if err != nil {
		return 1, fmt.Errorf("creating run record: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var emit func(agent.Event)
	var r *renderer
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		emit = func(e agent.Event) {
			rec.Event(e)
			if !e.Partial {
				if e.Time.IsZero() {
					e.Time = time.Now()
				}
				_ = enc.Encode(e)
			}
		}
	} else {
		r = newRenderer(thinking)
		emit = func(e agent.Event) { rec.Event(e); r.Event(e) }
	}

	res := b.Run(ctx, task, emit)
	if err := rec.Close(res); err != nil {
		fmt.Fprintln(os.Stderr, "hx: saving run:", err)
	}
	if jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"run_id": rec.ID(), "result": res})
	} else {
		r.Summary(rec.ID(), res)
	}
	if res.Status != agent.StatusOK {
		return 1, nil
	}
	return 0, nil
}

func cmdBackends() error {
	for _, name := range agent.Names() {
		b, _ := agent.Get(name)
		src, err := b.Detect()
		if err != nil {
			fmt.Printf("  ✗ %-9s %s\n", name, err)
		} else {
			fmt.Printf("  ✓ %-9s %s\n", name, src)
		}
	}
	return nil
}

func cmdRuns(args []string) error {
	fs := flag.NewFlagSet("runs", flag.ContinueOnError)
	n := fs.Int("n", 20, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	recs, err := runlog.List(*n)
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		fmt.Println("no runs yet")
		return nil
	}
	for _, rec := range recs {
		status := string(rec.Result.Status)
		if !rec.Done {
			status = "running?"
		}
		fmt.Printf("%s  %-8s %-9s %7s  %s\n", rec.ID, rec.Backend, status,
			rec.Result.Duration.Round(time.Second), tools.OneLine(rec.Task.Prompt, 70))
	}
	return nil
}

func cmdShow(args []string) error {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: hx show [--json] <run-id>")
	}
	id := fs.Arg(0)
	rec, err := runlog.Load(id)
	if err != nil {
		return err
	}
	events, err := runlog.Events(id)
	if err != nil {
		return err
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"record": rec, "events": events})
	}
	r := newRenderer(true)
	fmt.Printf("%s\n%s · %s · mode %s\n\n", r.c(bold, tools.OneLine(rec.Task.Prompt, 200)), rec.Backend, rec.Task.Dir, rec.Task.Mode)
	for _, e := range events {
		r.Event(e)
	}
	if rec.Done {
		r.Summary(rec.ID, rec.Result)
	}
	return nil
}

// cmdMCP serves hx over stdio until the host closes the pipe. stdout is the
// protocol channel, so nothing else may print there.
func cmdMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	dir := fs.String("dir", ".", "")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errors.New("invalid arguments")
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q; the workspace is set with --dir", fs.Arg(0))
	}
	ws, err := tools.NewWorkspace(*dir)
	if err != nil {
		return fmt.Errorf("--dir %s: %w", *dir, err)
	}
	abs := ws.Root
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	fmt.Fprintln(os.Stderr, "hx mcp: serving on stdio, workspace", abs)
	if err := hxmcp.NewServer(abs).Serve(ctx, os.Stdin, os.Stdout); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func cmdBatch(args []string) (int, error) {
	if len(args) >= 1 && args[0] == "rm" {
		if len(args) != 2 {
			return 2, errors.New("usage: hx batch rm <batch-id>")
		}
		res, err := batch.LoadResult(args[1])
		if err != nil {
			return 1, err
		}
		if err := batch.Remove(res); err != nil {
			return 1, err
		}
		fmt.Printf("removed the worktrees and branches of batch %s (%d tasks)\n", res.ID, len(res.Tasks))
		return 0, nil
	}
	fs := flag.NewFlagSet("batch", flag.ContinueOnError)
	parallel := fs.Int("parallel", 0, "")
	allowDirty := fs.Bool("allow-dirty", false, "")
	jsonOut := fs.Bool("json", false, "")
	var verbose bool
	for _, name := range []string{"v", "verbose"} {
		fs.BoolVar(&verbose, name, false, "")
	}
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return 2, nil
	}
	if fs.NArg() != 1 {
		return 2, errors.New("usage: hx batch [flags] <tasks.json>")
	}
	spec, err := batch.LoadSpec(fs.Arg(0))
	if err != nil {
		return 2, err
	}
	if *parallel > 0 {
		spec.Parallel = *parallel
	}
	if *allowDirty {
		spec.AllowDirty = true
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	r := newRenderer(false)
	if *jsonOut {
		r.out = os.Stderr
	}
	res, err := batch.Run(ctx, spec, func(e batch.Event) {
		if e.Kind == "tool" && !verbose {
			return
		}
		r.Line(fmt.Sprintf("[%s] %s", e.Task, e.Text))
	})
	if err != nil && len(res.Tasks) == 0 {
		return 1, err
	}
	if err != nil {
		// The batch ran; only its record could not be written.
		r.Line(r.c(red, "error: "+err.Error()))
	}

	passed := 0
	for _, t := range res.Tasks {
		if t.Status == batch.StatusPass {
			passed++
		}
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	} else {
		r.Line("")
		for _, t := range res.Tasks {
			last := "-"
			if n := len(t.Attempts); n > 0 {
				last = t.Attempts[n-1].Backend
			}
			status := string(t.Status)
			if t.Status != batch.StatusPass {
				status = r.c(red, status)
			}
			r.Line(fmt.Sprintf("  %-16s %-9s %-9s %d attempt(s)  %-24s %s", t.ID, status, last, len(t.Attempts), t.Branch, t.Duration.Round(time.Second)))
		}
		r.Line(r.c(dim, fmt.Sprintf("── batch %s · %d/%d passed · %s ──", res.ID, passed, len(res.Tasks), res.Duration.Round(time.Second))))
		r.Line(r.c(dim, fmt.Sprintf("worktrees under %s · once merged or abandoned, remove them with: hx batch rm %s", filepath.Join(res.Dir, ".hx", "worktrees"), res.ID)))
	}
	if passed != len(res.Tasks) {
		return 1, nil
	}
	return 0, nil
}
