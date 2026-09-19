// Package batch is the orchestrator: it runs a set of tasks against one git
// repository, N agents at a time, each in its own worktree on its own branch.
// After an agent finishes, gate commands (build, test) run in that worktree;
// a task whose run or gates fail is retried on the next backend in its list.
// Every attempt is an ordinary recorded run, so the eval layer can score it.
package batch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/mcp"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/tools"
)

// Spec is a batch of tasks to run against one repository. Task fields
// override the Spec defaults of the same name.
type Spec struct {
	Dir      string   `json:"dir,omitempty"`      // any path inside the repository; default: the spec file's directory
	Parallel int      `json:"parallel,omitempty"` // agents at once; default 2
	Backend  string   `json:"backend,omitempty"`  // default: api when credentials exist, else claude
	Model    string   `json:"model,omitempty"`
	Mode     string   `json:"mode,omitempty"` // default auto; nothing can approve prompts in a batch
	MaxTurns int      `json:"max_turns,omitempty"`
	Effort   string   `json:"effort,omitempty"`
	MCP      []string `json:"mcp,omitempty"`   // MCP config files, relative to Dir
	Gate     []string `json:"gate,omitempty"`  // commands that must exit 0 in the worktree after the agent finishes
	Retry    []string `json:"retry,omitempty"` // backends to try, in order, after a failed attempt
	Tasks    []Task   `json:"tasks"`
}

type Task struct {
	ID       string   `json:"id,omitempty"` // branch hx/<id> and worktree .hx/worktrees/<id>; default t1, t2, ...
	Prompt   string   `json:"prompt"`
	Backend  string   `json:"backend,omitempty"`
	Model    string   `json:"model,omitempty"`
	Mode     string   `json:"mode,omitempty"`
	MaxTurns int      `json:"max_turns,omitempty"`
	Effort   string   `json:"effort,omitempty"`
	Gate     []string `json:"gate,omitempty"`
	Retry    []string `json:"retry,omitempty"`
}

type Status string

const (
	StatusPass     Status = "pass"     // the run finished and every gate exited 0
	StatusFail     Status = "fail"     // every attempt failed its run or a gate
	StatusError    Status = "error"    // the task could not be set up (branch taken, git failure)
	StatusCanceled Status = "canceled" // interrupted
)

// Result is the record of one batch, saved under $HX_HOME/batches.
type Result struct {
	ID       string        `json:"id"`
	Started  time.Time     `json:"started"`
	Dir      string        `json:"dir"`  // repository root
	Base     string        `json:"base"` // commit every worktree branched from
	Parallel int           `json:"parallel"`
	Tasks    []TaskResult  `json:"tasks"`
	Duration time.Duration `json:"duration_ns"`
}

type TaskResult struct {
	ID       string        `json:"id"`
	Prompt   string        `json:"prompt"`
	Status   Status        `json:"status"`
	Branch   string        `json:"branch"`
	Worktree string        `json:"worktree"`
	Commit   string        `json:"commit,omitempty"` // the agent's changes; empty when it changed nothing
	Attempts []Attempt     `json:"attempts"`
	Error    string        `json:"error,omitempty"`
	Duration time.Duration `json:"duration_ns"`
}

type Attempt struct {
	Backend string       `json:"backend"`
	RunID   string       `json:"run_id,omitempty"`
	Result  agent.Result `json:"result"`
	Gates   []GateResult `json:"gates,omitempty"`
	Error   string       `json:"error,omitempty"`
}

type GateResult struct {
	Command  string        `json:"command"`
	ExitCode int           `json:"exit_code"`
	Output   string        `json:"output"` // last part of combined stdout/stderr
	Duration time.Duration `json:"duration_ns"`
}

// Event is progress for a caller to print. Kind is one of start, tool, run,
// gate, retry, done.
type Event struct {
	Task string
	Kind string
	Text string
}

const gateTimeout = 15 * time.Minute

var idRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// LoadSpec reads a spec file. A relative Dir is taken from the file's
// directory, which is also the default.
func LoadSpec(path string) (Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Spec{}, err
	}
	var spec Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return Spec{}, fmt.Errorf("%s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return Spec{}, err
	}
	if !filepath.IsAbs(spec.Dir) {
		spec.Dir = filepath.Join(filepath.Dir(abs), spec.Dir)
	}
	if err := spec.normalize(); err != nil {
		return Spec{}, fmt.Errorf("%s: %w", path, err)
	}
	return spec, nil
}

func (s *Spec) normalize() error {
	if s.Dir == "" {
		s.Dir = "."
	}
	if s.Parallel <= 0 {
		s.Parallel = 2
	}
	if s.Mode == "" {
		s.Mode = string(agent.ModeAuto)
	}
	if _, err := agent.ParseMode(s.Mode); err != nil {
		return err
	}
	if len(s.Tasks) == 0 {
		return errors.New("no tasks")
	}
	seen := map[string]bool{}
	for i := range s.Tasks {
		t := &s.Tasks[i]
		if strings.TrimSpace(t.Prompt) == "" {
			return fmt.Errorf("task %d: empty prompt", i+1)
		}
		if t.ID == "" {
			t.ID = fmt.Sprintf("t%d", i+1)
		}
		if !idRE.MatchString(t.ID) {
			return fmt.Errorf("task id %q: use letters, digits, '.', '_' and '-'", t.ID)
		}
		if seen[t.ID] {
			return fmt.Errorf("duplicate task id %q", t.ID)
		}
		seen[t.ID] = true
		if t.Mode != "" {
			if _, err := agent.ParseMode(t.Mode); err != nil {
				return fmt.Errorf("task %s: %w", t.ID, err)
			}
		}
	}
	return nil
}

// Run executes the batch and saves its record. Tasks run through a pool of
// Parallel workers; cancelling ctx stops running agents and skips the rest.
func Run(ctx context.Context, spec Spec, notify func(Event)) (Result, error) {
	started := time.Now()
	if err := spec.normalize(); err != nil {
		return Result{}, err
	}
	if notify == nil {
		notify = func(Event) {}
	}
	root, err := gitOut(ctx, spec.Dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return Result{}, err
	}
	root = filepath.Clean(filepath.FromSlash(root))
	base, err := gitOut(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return Result{}, err
	}
	var mcpPaths []string
	for _, p := range spec.MCP {
		if !filepath.IsAbs(p) {
			p = filepath.Join(spec.Dir, p)
		}
		mcpPaths = append(mcpPaths, p)
	}
	if mcpPaths, err = mcp.ConfigPaths(mcpPaths); err != nil {
		return Result{}, err
	}
	// Keep the worktrees out of the repository's status.
	wtRoot := filepath.Join(root, ".hx", "worktrees")
	if err := os.MkdirAll(wtRoot, 0o755); err != nil {
		return Result{}, err
	}
	if _, err := os.Stat(filepath.Join(root, ".hx", ".gitignore")); err != nil {
		_ = os.WriteFile(filepath.Join(root, ".hx", ".gitignore"), []byte("*\n"), 0o644)
	}

	res := Result{ID: runlog.NewID(), Started: started, Dir: root, Base: base, Parallel: spec.Parallel, Tasks: make([]TaskResult, len(spec.Tasks))}
	idx := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < spec.Parallel; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range idx {
				res.Tasks[i] = runTask(ctx, &spec, spec.Tasks[i], root, base, mcpPaths, notify)
			}
		}()
	}
	for i := range spec.Tasks {
		idx <- i
	}
	close(idx)
	wg.Wait()
	res.Duration = time.Since(started)
	return res, save(res)
}

func runTask(ctx context.Context, spec *Spec, t Task, root, base string, mcpPaths []string, notify func(Event)) TaskResult {
	started := time.Now()
	tr := TaskResult{ID: t.ID, Prompt: t.Prompt, Branch: "hx/" + t.ID, Worktree: filepath.Join(root, ".hx", "worktrees", t.ID), Attempts: []Attempt{}}
	finish := func(status Status, text string) TaskResult {
		tr.Status = status
		tr.Duration = time.Since(started)
		notify(Event{Task: t.ID, Kind: "done", Text: text})
		return tr
	}
	fail := func(err error) TaskResult {
		tr.Error = err.Error()
		return finish(StatusError, "error: "+err.Error())
	}
	if ctx.Err() != nil {
		return finish(StatusCanceled, "canceled")
	}

	backends := []string{firstOf(t.Backend, spec.Backend, agent.DefaultBackendName())}
	backends = append(backends, firstList(t.Retry, spec.Retry)...)
	gates := firstList(t.Gate, spec.Gate)
	mode, _ := agent.ParseMode(firstOf(t.Mode, spec.Mode))

	if _, err := gitOut(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+tr.Branch); err == nil {
		return fail(fmt.Errorf("branch %s already exists; remove it or pick another task id", tr.Branch))
	}
	if _, err := os.Stat(tr.Worktree); err == nil {
		return fail(fmt.Errorf("worktree %s already exists; remove it or pick another task id", tr.Worktree))
	}

	for i, name := range backends {
		b, err := agent.Get(name)
		if err != nil {
			return fail(err)
		}
		if i > 0 {
			notify(Event{Task: t.ID, Kind: "retry", Text: "retrying on " + name})
			if err := removeWorktree(root, tr); err != nil {
				return fail(err)
			}
		}
		tr.Commit = ""
		if _, err := gitOut(ctx, root, "worktree", "add", "-q", "-b", tr.Branch, tr.Worktree, base); err != nil {
			return fail(err)
		}
		notify(Event{Task: t.ID, Kind: "start", Text: fmt.Sprintf("%s in %s", name, tr.Worktree)})

		task := agent.Task{
			Prompt: t.Prompt, Dir: tr.Worktree, Mode: mode, MCP: mcpPaths,
			Model: firstOf(t.Model, spec.Model), Effort: firstOf(t.Effort, spec.Effort),
			MaxTurns: t.MaxTurns,
		}
		if task.MaxTurns == 0 {
			task.MaxTurns = spec.MaxTurns
		}
		att := Attempt{Backend: name}
		rec, err := runlog.Create(name, task)
		if err != nil {
			return fail(fmt.Errorf("creating run record: %w", err))
		}
		att.RunID = rec.ID()
		att.Result = b.Run(ctx, task, func(e agent.Event) {
			rec.Event(e)
			if e.Kind == agent.EventToolCall {
				notify(Event{Task: t.ID, Kind: "tool", Text: strings.TrimSpace("→ " + e.Tool + " " + oneLine(e.Text, 120))})
			}
		})
		if err := rec.Close(att.Result); err != nil {
			att.Error = "saving run: " + err.Error()
		}
		text := fmt.Sprintf("%s · %d turns · run %s", att.Result.Status, att.Result.Turns, att.RunID)
		if att.Result.Error != "" {
			text += " · " + oneLine(att.Result.Error, 160)
		}
		notify(Event{Task: t.ID, Kind: "run", Text: text})

		ok := att.Result.Status == agent.StatusOK
		if ok {
			for _, g := range gates {
				gr := runGate(ctx, tr.Worktree, g)
				att.Gates = append(att.Gates, gr)
				if gr.ExitCode != 0 {
					ok = false
					notify(Event{Task: t.ID, Kind: "gate", Text: fmt.Sprintf("✗ %s (exit %d)", g, gr.ExitCode)})
					break
				}
				notify(Event{Task: t.ID, Kind: "gate", Text: "✓ " + g})
			}
		}
		// The branch holds whatever the agent left, pass or fail, so the
		// worktree can be inspected or removed without losing work.
		if sha, err := commitChanges(tr.Worktree, t); err != nil {
			att.Error = strings.TrimSpace(att.Error + "\ncommit: " + err.Error())
		} else {
			tr.Commit = sha
		}
		tr.Attempts = append(tr.Attempts, att)

		switch {
		case ok:
			text := "pass · branch " + tr.Branch
			if tr.Commit != "" {
				text += " · commit " + short(tr.Commit)
			}
			return finish(StatusPass, text)
		case ctx.Err() != nil:
			return finish(StatusCanceled, "canceled")
		}
	}
	return finish(StatusFail, fmt.Sprintf("fail after %d attempt(s) · worktree kept at %s", len(tr.Attempts), tr.Worktree))
}

func runGate(ctx context.Context, dir, command string) GateResult {
	started := time.Now()
	_, argv := tools.Shell()
	gctx, cancel := context.WithTimeout(ctx, gateTimeout)
	defer cancel()
	args := argv(command)
	cmd := exec.CommandContext(gctx, args[0], args[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	gr := GateResult{Command: command, Output: tail(string(out), 4000), Duration: time.Since(started)}
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		gr.ExitCode = exit.ExitCode()
	default:
		gr.ExitCode = -1
		gr.Output = strings.TrimSpace(gr.Output + "\n" + err.Error())
	}
	if gctx.Err() != nil && gr.ExitCode == 0 {
		gr.ExitCode = -1
	}
	return gr
}

// commitChanges commits everything the agent left in the worktree and returns
// the commit, or "" when nothing changed. It runs after the agent, so it uses
// its own context: a cancelled batch must still leave a consistent branch.
func commitChanges(wt string, t Task) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	status, err := gitOut(ctx, wt, "status", "--porcelain")
	if err != nil || status == "" {
		return "", err
	}
	if _, err := gitOut(ctx, wt, "add", "-A"); err != nil {
		return "", err
	}
	if _, err := gitOut(ctx, wt, "commit", "-q", "-m", t.ID+": "+oneLine(t.Prompt, 60)); err != nil {
		return "", err
	}
	return gitOut(ctx, wt, "rev-parse", "HEAD")
}

// Remove deletes the worktrees and branches a batch created. The record and
// the run logs stay.
func Remove(res Result) error {
	var errs []error
	for _, tr := range res.Tasks {
		if tr.Worktree == "" {
			continue
		}
		if err := removeWorktree(res.Dir, tr); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", tr.ID, err))
		}
	}
	return errors.Join(errs...)
}

func removeWorktree(root string, tr TaskResult) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := os.Stat(tr.Worktree); err == nil {
		if _, err := gitOut(ctx, root, "worktree", "remove", "--force", tr.Worktree); err != nil {
			return err
		}
	} else {
		_, _ = gitOut(ctx, root, "worktree", "prune")
	}
	if _, err := gitOut(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+tr.Branch); err == nil {
		if _, err := gitOut(ctx, root, "branch", "-D", tr.Branch); err != nil {
			return err
		}
	}
	return nil
}

func batchesDir() string { return filepath.Join(runlog.Home(), "batches") }

func save(res Result) error {
	if err := os.MkdirAll(batchesDir(), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(batchesDir(), res.ID+".json"), data, 0o644)
}

// LoadResult reads a saved batch record.
func LoadResult(id string) (Result, error) {
	var res Result
	if id == "" || strings.ContainsAny(id, `/\`) {
		return res, fmt.Errorf("invalid batch id %q", id)
	}
	data, err := os.ReadFile(filepath.Join(batchesDir(), id+".json"))
	if err != nil {
		return res, err
	}
	return res, json.Unmarshal(data, &res)
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), text)
	}
	return text, nil
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstList(lists ...[]string) []string {
	for _, l := range lists {
		if l != nil {
			return l
		}
	}
	return nil
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// oneLine collapses whitespace (errors are often multi-line JSON) and truncates.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		s = string(r[:n]) + "…"
	}
	return s
}
