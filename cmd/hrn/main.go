// Command hrn is a personal agent harness: one CLI that runs a coding task on
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
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/eval"
	"github.com/cristiangirlea/harness/internal/followup"
	"github.com/cristiangirlea/harness/internal/hrnmcp"
	"github.com/cristiangirlea/harness/internal/limits"
	"github.com/cristiangirlea/harness/internal/mcp"
	"github.com/cristiangirlea/harness/internal/report"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/tools"
)

const usage = `hrn — personal agent harness

Usage:
  hrn run [flags] <prompt...>     run a task (prompt from args, or stdin when omitted or "-")
  hrn exec --protocol v1          run one recorded task over the supervisor JSONL protocol
  hrn backends                    show which backends are usable
  hrn remote submit|show|events|cancel [flags]
                                 use the durable control API
  hrn knowledge import|search|inspect|active|activate|disable|rollback request.json
                                 manage approved project knowledge through the control API
  hrn version                     print the hrn version, Go version and commit
  hrn runs [-n N]                 list recent runs
  hrn runs prune --older-than AGE remove old runs and records (--keep N, --dry-run)
  hrn show [--json] <run-id>      replay a recorded run
  hrn show -f <run-id>            follow a run as it happens, until it ends (--follow)
  hrn watch [--addr HOST:PORT] [--lan[=NAME]]
                                 a local page that follows runs and batches live (default 127.0.0.1:8089,
                                 loopback only); --lan also serves it, read-only and without a login,
                                 on this computer's private network addresses, for a phone, with a QR
                                 code; --lan=NAME only on one adapter (its name or address)
  hrn bot [--addr HOST:PORT]      Ada, hrn watch's bot, in a small window: says what hrn is doing in plain UK
                                 English, out loud and as notifications, with skip, snooze and a few
                                 spoken commands; serves hrn watch on loopback unless one already does
  hrn mcp [--dir PATH] [--allow-dangerous-runs]
                                 serve hrn as an MCP server on stdio; --dir is the default workspace (default: current
                                 directory); allow_dangerous is refused unless --allow-dangerous-runs is given
  hrn batch [flags] <tasks.json>  run tasks in parallel, each in its own git worktree, with gates and retries
  hrn batch show [--json] <id>    reprint a batch's table, while it runs or afterwards
  hrn batch diff <id> <task-id>   what one task changed, as a diff on stdout
  hrn batch merge [--squash] <id> [task-id...]
                                 merge the branches of the tasks that passed into the current branch
  hrn batch rm <batch-id>         remove the worktrees and branches a batch created
  hrn eval [flags] <eval.json>    run a task set across a matrix of backends, models and efforts
  hrn eval show <eval-id>         reprint a saved eval
  hrn eval list [-n N]            list saved evals, newest first
  hrn eval diff [--json] <a> <b>  two evals side by side: did the change help
  hrn report [flags]              tables over the finished runs and the batches: pass rate, duration, turns, cost
  hrn gc [--apply] [--grace D] [--idle D] [--min-idle D] [--free-below SIZE] [PATH...]
                                 list worktrees whose work is merged (pull request merged at their commit, or a
                                 finished batch's commit on origin's default branch) and build caches (CACHEDIR.TAG)
                                 idle past --idle or, on a drive below --free-below, past --min-idle; --apply
                                 removes them, and the worktrees' branches. PATH is a repository or a folder of
                                 them (default: the current one)

Run flags:
  -b, --backend NAME   api | claude | codex | gemini | opencode (default: api if credentials exist, else claude)
  -m, --model NAME     model id (api default: claude-opus-5)
  -C, --dir PATH       workspace directory (default: current directory)
      --mode MODE      readonly | ask | auto | yolo (default: ask)
      --max-turns N    stop after N model turns (default 50 for api)
      --effort LEVEL   low | medium | high | xhigh | max (api, codex and claude; unset uses backend default)
      --json           print events as JSON lines instead of formatted output
      --thinking       show model thinking summaries
      --mcp FILE       MCP config (Claude Code .mcp.json format); repeatable. ~/.hrn/mcp.json is loaded when present
      --no-instructions
                       skip the project instruction files (api backend)
      --no-context-edits
                       keep every tool result in the request instead of letting the server clear old ones (api backend)
      --continue ID    continue a finished run, on its backend or, with -b, on another one
      --timeout D      stop the run after D (e.g. 20m)
      --max-cost USD   stop the api loop before a turn that would pass this estimated cost
      --task-budget N  token ceiling the api backend gives the model to pace itself (min 20000)
      --allow-dangerous
                       lift the guard: agents may push, merge and deploy with your credentials
                       (every backend), and run_command may run sudo, rm -rf / and the like
      --no-switch      stay on claude or codex when it reaches its plan's usage limit, instead of
                       carrying on on the other one

Eval flags:
      --parallel N     cells at once (default: the file's value, else 4)
      --keep           keep the worktrees and branches instead of removing them
      --json           print the eval record as JSON (progress goes to stderr)
  -v, --verbose        show each agent's tool calls

Batch flags:
      --parallel N     agents at once (default: the file's value, else 4)
      --allow-dirty    start even though the repository has uncommitted changes
      --squash         merge: one commit per task instead of a merge commit
      --json           print the batch record as JSON (progress goes to stderr)
  -v, --verbose        show each agent's tool calls

Report flags:
      --since WINDOW   only runs and batches started within the window, e.g. 7d, 36h, 90m (default: all)
      --by KEY         group runs by backend (default), model, dir or purpose
      --json           print the tables as JSON

Remote commands use HRN_SERVER_URL, HRN_TENANT_TOKEN, and optional HRN_SERVER_CA_FILE.
Runs are recorded under ~/.hrn/runs (override with HRN_HOME).
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
	case "exec":
		code, err = cmdExec(os.Args[2:])
	case "backends":
		err = cmdBackends()
	case "remote":
		err = runRemote(os.Args[2:], os.Stdout)
	case "knowledge":
		err = runKnowledge(os.Args[2:], os.Stdout)
	case "runs":
		err = cmdRuns(os.Args[2:])
	case "show":
		err = cmdShow(os.Args[2:])
	case "mcp":
		err = cmdMCP(os.Args[2:])
	case "batch":
		code, err = cmdBatch(os.Args[2:])
	case "eval":
		code, err = cmdEval(os.Args[2:])
	case "report":
		err = cmdReport(os.Args[2:])
	case "watch":
		err = cmdWatch(os.Args[2:])
	case "bot":
		err = cmdBot(os.Args[2:])
	case "gc":
		code, err = cmdGC(os.Args[2:])
	case "version", "--version", "-V":
		info, _ := debug.ReadBuildInfo()
		fmt.Println(versionLine(info))
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hrn:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func cmdRun(args []string) (int, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var backend, dir string
	var jsonOut, thinking bool
	// Every option is a flag named after its json name; the struct is the
	// list. The mode default belongs to this command, not to the struct.
	opts := agent.Options{Mode: agent.ModeAsk}
	agent.BindFlags(fs, &opts)
	fs.StringVar(&opts.Model, "m", "", "")
	for _, name := range []string{"b", "backend"} {
		fs.StringVar(&backend, name, "", "")
	}
	for _, name := range []string{"C", "dir"} {
		fs.StringVar(&dir, name, ".", "")
	}
	fs.BoolVar(&jsonOut, "json", false, "")
	fs.BoolVar(&thinking, "thinking", false, "")
	continueID := fs.String("continue", "", "")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return 2, nil
	}

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	// A backend whose plan limit is remembered hands the run to its partner
	// from the start, a continued one too, so this is settled before the
	// plan. A model typed for it means nothing to the partner.
	var routed string
	if !opts.NoSwitch {
		if backend, routed = limits.Choose(backend, *continueID); routed != "" {
			opts.Model = ""
		}
	}

	// A continued run inherits the earlier run's backend, workspace, mode and
	// model for anything the user did not type this time; the plan says what
	// the backend picks up and what it is handed as a transcript.
	var plan followup.Plan
	if *continueID != "" {
		req := followup.Request{RunID: *continueID, Backend: backend}
		if set["C"] || set["dir"] {
			req.Dir = dir
		}
		if (set["m"] || set["model"]) && routed == "" {
			req.Model = opts.Model
		}
		var err error
		if plan, err = followup.Make(req); err != nil {
			return 2, err
		}
		backend, dir, opts.Model = plan.Backend, plan.Dir, plan.Model
		if !set["mode"] && plan.Prev.Task.Mode != "" {
			opts.Mode = plan.Prev.Task.Mode
		}
	}

	if err := opts.Validate(); err != nil {
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

	task := agent.Task{Prompt: prompt, Dir: absDir, Options: opts}
	if task.MCP, err = mcp.ConfigPaths(opts.MCP); err != nil {
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

	rec, err := runlog.CreateContinuing(backend, task, *continueID)
	if err != nil {
		return 1, fmt.Errorf("creating run record: %w", err)
	}
	// The api backend saves the conversation here, so this run can be continued.
	task.RunDir = rec.Dir()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
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

	if routed != "" {
		emit(agent.Event{Time: time.Now(), Kind: agent.EventInfo, Text: routed})
	}
	if warning := plan.ContextWarning(); warning != "" {
		fmt.Fprintln(os.Stderr, warning)
	}
	plan.Apply(&task, rec.Dir(), emit)
	res := agent.Run(ctx, b, task, emit)
	if err := rec.Close(res); err != nil {
		fmt.Fprintln(os.Stderr, "hrn: "+runlog.CloseMessage(err))
	}
	finished := func(id string, res agent.Result) {
		if jsonOut {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"run_id": id, "result": res})
		} else {
			r.Summary(id, res)
		}
	}
	finished(rec.ID(), res)

	// A run stopped by its plan's limit is carried on by the partner backend,
	// once: a partner that is limited too would only hand it back.
	next, why, err := limits.After(rec.ID(), backend, task, res)
	switch {
	case err != nil:
		fmt.Fprintln(os.Stderr, "hrn:", err)
	case next == nil && why != "":
		fmt.Fprintln(os.Stderr, "hrn:", why)
	case next != nil:
		nb, err := agent.Get(next.To)
		if err != nil {
			return 1, err
		}
		nrec, err := runlog.CreateContinuing(next.To, next.Task, rec.ID())
		if err != nil {
			return 1, fmt.Errorf("creating run record: %w", err)
		}
		next.Task.RunDir = nrec.Dir()
		rec = nrec // the renderer and the encoder write to the new record now
		emit(agent.Event{Time: time.Now(), Kind: agent.EventInfo, Text: next.Why})
		next.Plan.Apply(&next.Task, nrec.Dir(), emit)
		res = agent.Run(ctx, nb, next.Task, emit)
		if err := nrec.Close(res); err != nil {
			fmt.Fprintln(os.Stderr, "hrn: "+runlog.CloseMessage(err))
		}
		// Limited too: remembered, so the next run does not try it first.
		if err := limits.Note(next.To, res); err != nil {
			fmt.Fprintln(os.Stderr, "hrn: the limit was not remembered:", err)
		}
		finished(nrec.ID(), res)
	}
	if res.Status != agent.StatusOK {
		return 1, nil
	}
	return 0, nil
}

func cmdBackends() error {
	now := time.Now()
	for _, name := range agent.Names() {
		b, _ := agent.Get(name)
		src, err := b.Detect()
		if err != nil {
			fmt.Printf("  ✗ %-9s %s\n", name, err)
		} else {
			fmt.Printf("  ✓ %-9s %s\n", name, src)
		}
		if s := limits.Status(name, now); s != "" {
			fmt.Printf("    %-9s %s\n", "", s)
		}
	}
	return nil
}

func cmdRuns(args []string) error {
	if len(args) > 0 && args[0] == "prune" {
		return cmdPrune(args[1:])
	}
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
		fmt.Printf("%s  %-8s %-9s %7s  %s\n", rec.ID, printable(rec.Backend), status,
			rec.Result.Duration.Round(time.Second), printable(tools.OneLine(rec.Task.Prompt, 70)))
	}
	return nil
}

func cmdShow(args []string) error {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "")
	follow := fs.Bool("follow", false, "")
	fs.BoolVar(follow, "f", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *jsonOut && *follow {
		return errors.New("--follow prints the run as it happens and does not combine with --json")
	}
	if fs.NArg() != 1 {
		return errors.New("usage: hrn show [--json | -f] <run-id>")
	}
	id := fs.Arg(0)
	if *follow {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
		defer stop()
		return followRun(ctx, newRenderer(true), id, 250*time.Millisecond)
	}
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
	showHeader(r, rec)
	for _, e := range events {
		r.Event(e)
	}
	if rec.Done {
		r.Summary(rec.ID, rec.Result)
	}
	return nil
}

// showHeader is what hrn show prints above a run's events.
func showHeader(r *renderer, rec runlog.Record) {
	head := fmt.Sprintf("%s · %s · mode %s", rec.Backend, rec.Task.Dir, rec.Task.Mode)
	if rec.Continues != "" {
		head += " · continues " + rec.Continues
	}
	if rec.Result.Session != "" {
		head += " · session " + rec.Result.Session
	}
	head = printable(head)
	r.Line(r.c(bold, printable(tools.OneLine(rec.Task.Prompt, 200))))
	r.Line(head)
	if rec.RecordError != "" {
		r.Line("run record incomplete: " + printable(strings.Join(strings.Fields(rec.RecordError), " ")))
	}
	r.Line("")
}

// cmdMCP serves hrn over stdio until the host closes the pipe. stdout is the
// protocol channel, so nothing else may print there.
func cmdMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	dir := fs.String("dir", ".", "")
	// The server's caller is an agent; lifting the guard for its runs is the
	// operator's decision, taken here and not in a tool call.
	dangerous := fs.Bool("allow-dangerous-runs", false, "")
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
	newServer := hrnmcp.NewServer
	if *dangerous {
		newServer = hrnmcp.NewDangerousServer
	}
	ws, err := tools.NewWorkspace(*dir)
	if err != nil {
		return fmt.Errorf("--dir %s: %w", *dir, err)
	}
	abs := ws.Root
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	fmt.Fprintln(os.Stderr, "hrn mcp: serving on stdio, workspace", abs)
	if err := newServer(abs).Serve(ctx, os.Stdin, os.Stdout); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

func cmdEval(args []string) (int, error) {
	if len(args) >= 1 && args[0] == "list" {
		fs := flag.NewFlagSet("eval list", flag.ContinueOnError)
		n := fs.Int("n", 20, "")
		if err := fs.Parse(args[1:]); err != nil {
			return 2, err
		}
		if fs.NArg() > 0 {
			return 2, errors.New("usage: hrn eval list [-n N]")
		}
		return 0, listEvals(os.Stdout, *n)
	}
	if len(args) >= 1 && args[0] == "diff" {
		fs := flag.NewFlagSet("eval diff", flag.ContinueOnError)
		jsonOut := fs.Bool("json", false, "")
		if err := fs.Parse(args[1:]); err != nil {
			return 2, err
		}
		if fs.NArg() != 2 {
			return 2, errors.New("usage: hrn eval diff [--json] <eval-id> <eval-id>")
		}
		a, err := eval.LoadRecord(fs.Arg(0))
		if err != nil {
			return 1, err
		}
		b, err := eval.LoadRecord(fs.Arg(1))
		if err != nil {
			return 1, err
		}
		d := eval.Compare(a, b)
		if *jsonOut {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return 0, enc.Encode(d)
		}
		printEvalDiff(newRenderer(false), d)
		return 0, nil
	}
	if len(args) >= 1 && args[0] == "show" {
		if len(args) != 2 {
			return 2, errors.New("usage: hrn eval show <eval-id>")
		}
		rec, err := eval.LoadRecord(args[1])
		if err != nil {
			return 1, err
		}
		printEval(newRenderer(false), rec)
		return 0, nil
	}
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	parallel := fs.Int("parallel", 0, "")
	keep := fs.Bool("keep", false, "")
	jsonOut := fs.Bool("json", false, "")
	var verbose bool
	for _, name := range []string{"v", "verbose"} {
		fs.BoolVar(&verbose, name, false, "")
	}
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return 2, errors.New("invalid arguments")
	}
	if fs.NArg() != 1 {
		return 2, errors.New("usage: hrn eval [flags] <eval.json>")
	}
	spec, err := eval.LoadSpec(fs.Arg(0))
	if err != nil {
		return 2, err
	}
	if *parallel > 0 {
		spec.Parallel = *parallel
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	r := newRenderer(false)
	if *jsonOut {
		r.out = os.Stderr
	}
	rec, res, err := eval.Run(ctx, spec, func(e batch.Event) {
		if e.Kind == "tool" && !verbose {
			return
		}
		r.Line(printable(fmt.Sprintf("[%s] %s", e.Task, e.Text)))
	})
	if err != nil && rec.ID == "" {
		return 1, err
	}
	failed := err != nil
	if failed {
		r.Line(r.c(red, "error: "+err.Error()))
	}

	// The worktrees are the evidence; they go only when the record is safe.
	if !*keep {
		if err := batch.Remove(res); err != nil {
			r.Line(r.c(red, "removing worktrees: "+err.Error()))
		}
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rec)
	} else {
		printEval(r, rec)
		if *keep {
			r.Line(r.c(dim, "worktrees kept; remove them with: hrn batch rm "+rec.BatchID))
		}
	}
	// A failing variant is data; a failing eval is not.
	if failed {
		return 1, nil
	}
	return 0, nil
}

// printEval writes the two tables: what each variant did, and how each task
// scored under it.
func printEval(r *renderer, rec eval.Record) {
	r.Line("")
	judged := rec.Spec.Judge != nil
	head := fmt.Sprintf("  %-20s %6s %9s %12s %10s %7s %7s %9s", "variant", "pass", "attempts", "median cost", "median time", "turns", "tools", "context")
	if judged {
		head += fmt.Sprintf(" %6s", "judge")
	}
	r.Line(head)
	for _, v := range rec.Table.Variants {
		line := fmt.Sprintf("  %-20s %6s %9d %12s %10s %7s %7s %9s",
			v.Name, report.Share(v.PassRate()), v.Attempts,
			report.Money(v.MedianCostUSD, v.CostEstimate),
			v.MedianDuration.Round(time.Second), report.Number(v.MedianTurns),
			report.Number(v.MedianTools), report.Number(v.MedianContext))
		if judged {
			score := "-"
			if v.Judged > 0 {
				score = fmt.Sprintf("%.1f", v.MeanScore)
			}
			line += fmt.Sprintf(" %6s", score)
		}
		r.Line(line)
		var notes []string
		if len(v.Statuses) > 1 || v.Statuses["ok"] != v.Attempts {
			notes = append(notes, statuses(v.Statuses))
		}
		if v.Unchanged > 0 {
			notes = append(notes, fmt.Sprintf("%d of %d passes changed nothing", v.Unchanged, v.Passed))
		}
		if len(notes) > 0 {
			r.Line(r.c(dim, "  "+strings.Repeat(" ", 20)+strings.Join(notes, " · ")))
		}
	}

	if len(rec.Table.Tasks) > 0 {
		var names []string
		for _, v := range rec.Table.Variants {
			names = append(names, v.Name)
		}
		scores := map[string]string{}
		var tasks []string
		for _, row := range rec.Table.Tasks {
			if _, seen := scores[row.Task+"/"+row.Variant]; !seen {
				scores[row.Task+"/"+row.Variant] = row.Score()
			}
			if !slices.Contains(tasks, row.Task) {
				tasks = append(tasks, row.Task)
			}
		}
		r.Line("")
		head := fmt.Sprintf("  %-20s", "task")
		for _, n := range names {
			head += fmt.Sprintf(" %10s", n)
		}
		r.Line(head)
		for _, task := range tasks {
			line := fmt.Sprintf("  %-20s", task)
			for _, n := range names {
				line += fmt.Sprintf(" %10s", scores[task+"/"+n])
			}
			r.Line(line)
		}
	}

	cells, _, _, _ := rec.Totals()
	if judged {
		failed, estimate := 0, false
		for _, j := range rec.Judgements {
			if !j.Scored() {
				failed++
			}
			estimate = estimate || j.CostEstimate
		}
		note := fmt.Sprintf("judge: %d verdicts, %s", len(rec.Judgements), report.Money(rec.JudgeCostUSD, estimate))
		if failed > 0 {
			note += fmt.Sprintf(", %d could not score; see --json", failed)
		}
		r.Line(r.c(dim, note))
	}
	r.Line(r.c(dim, fmt.Sprintf("── eval %s · batch %s · %d cells · %s ──",
		rec.ID, rec.BatchID, cells, rec.Duration.Round(time.Second))))
}

// statuses renders a run-status histogram as "ok 3, timeout 1".
func statuses(h map[string]int) string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, h[k]))
	}
	return strings.Join(parts, ", ")
}
