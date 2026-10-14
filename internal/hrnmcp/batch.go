package hrnmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/mcp"
	"github.com/cristiangirlea/harness/internal/tools"
)

// The batch tools let a host fan work out: one call runs several tasks in
// their own git worktrees and branches, gates them, and returns; batch_show
// reads the results and diffs back; batch_merge lands the branches. They are
// the hrn batch subcommands with the same package calls behind them.

const specDescription = `A batch spec, the same JSON as an hrn batch file. Fields: tasks (required; each has prompt, and optionally id, backend, gate, retry, expect and any run option), and on the spec: dir, parallel (default 4), backend, gate (commands that must exit 0 in each worktree), retry (backends to try after a failed attempt), allow_dirty, worktrees ("slots", the default: each task runs in one of the repository's reusable slots, which keep their build output, and gives it back when it ends; "fresh": a new worktree per task, kept until hrn batch rm), and the run options (model, mode, max_turns, effort, timeout, max_cost_usd, ...) every task starts from. mode defaults to auto here too, because nothing can approve a prompt inside a batch; use yolo when a task must build or test. Unknown fields are refused, so a misspelt one cannot be silently ignored.`

func batchTool(defaultDir string, allowDangerous bool) mcp.Tool {
	return mcp.Tool{
		Name:  "batch",
		Title: "Run tasks in parallel worktrees",
		Description: "Run several coding tasks at once, each on its own branch hrn/<id> in its own git worktree, " +
			"then run the gate commands in each. Waits for every task and returns one line per task plus the batch id. " +
			"The repository must have no modified tracked files unless allow_dirty is set. " +
			"Read a task's changes with batch_show and land them with batch_merge.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"spec": map[string]any{"type": "object", "description": specDescription},
			},
			"required": []any{"spec"},
		},
		Annotations: &mcp.Annotations{ReadOnlyHint: mcp.Bool(false), OpenWorldHint: mcp.Bool(true)},
		Handler: func(ctx context.Context, call mcp.Call) (mcp.CallResult, error) {
			args, err := decode[struct {
				Spec json.RawMessage `json:"spec"`
			}](call.Arguments)
			if err != nil {
				return mcp.CallResult{}, err
			}
			spec, err := decodeSpec(args.Spec, defaultDir)
			if err != nil {
				return mcp.CallResult{}, err
			}
			if !allowDangerous && (spec.Options.AllowDangerous || slices.ContainsFunc(spec.Tasks, func(t batch.Task) bool { return t.AllowDangerous })) {
				return mcp.CallResult{}, errDangerousRefused
			}
			res, runErr := batch.Run(ctx, spec, func(e batch.Event) {
				if e.Kind == "tool" {
					return // one line per agent tool call would drown the host
				}
				call.Progress(fmt.Sprintf("[%s] %s", e.Task, tools.OneLine(e.Text, 200)))
			})
			if runErr != nil && res.ID == "" {
				return mcp.CallResult{}, runErr
			}
			body := batchSummary(res)
			if runErr != nil {
				body = "error: " + runErr.Error() + "\n\n" + body
			}
			return text(body, brief(res), runErr != nil || passed(res) < len(res.Tasks)), nil
		},
	}
}

// decodeSpec reads a spec the way hrn batch reads a file, except that a
// relative dir is taken from the server's workspace rather than a file's
// directory, and unknown fields are refused: the spec is written by a model,
// and a misspelt "gates" would otherwise run with no gate at all.
func decodeSpec(raw json.RawMessage, defaultDir string) (batch.Spec, error) {
	var spec batch.Spec
	if len(raw) == 0 || string(raw) == "null" {
		return spec, errors.New("spec is required")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return spec, fmt.Errorf("spec: %w", err)
	}
	if spec.Dir == "" {
		spec.Dir = defaultDir
	} else if !filepath.IsAbs(spec.Dir) {
		spec.Dir = filepath.Join(defaultDir, spec.Dir)
	}
	return spec, nil
}

func passed(res batch.Result) int {
	n := 0
	for _, t := range res.Tasks {
		if t.Status == batch.StatusPass {
			n++
		}
	}
	return n
}

// batchSummary is one line per task and a footer that says what to do next.
func batchSummary(res batch.Result) string {
	var b strings.Builder
	running := 0
	for _, t := range res.Tasks {
		fmt.Fprintf(&b, "%s: %s", t.ID, t.Status)
		if n := len(t.Attempts); n > 0 {
			last := t.Attempts[n-1]
			fmt.Fprintf(&b, " on %s after %d attempt(s)", last.Backend, n)
			if g := failedGateOf(last); g != nil {
				fmt.Fprintf(&b, "; gate `%s` exited %d", tools.OneLine(g.Command, 80), g.ExitCode)
			} else if last.Result.Error != "" {
				fmt.Fprintf(&b, "; %s", tools.OneLine(last.Result.Error, 160))
			}
		}
		if t.Error != "" {
			fmt.Fprintf(&b, "; %s", tools.OneLine(t.Error, 160))
		}
		switch {
		case t.Status == batch.StatusRunning:
			running++
		case t.Commit != "":
			fmt.Fprintf(&b, " · branch %s · commit %s", t.Branch, tools.ShortSHA(t.Commit))
		case t.Status == batch.StatusPass:
			b.WriteString(" · changed nothing")
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\nbatch %s · %d/%d passed", res.ID, passed(res), len(res.Tasks))
	if running > 0 {
		fmt.Fprintf(&b, " · %d still running, started %s ago", running, time.Since(res.Started).Round(time.Second))
	} else {
		fmt.Fprintf(&b, " · %s", res.Duration.Round(time.Second))
	}
	kept := "the worktrees stay"
	for _, t := range res.Tasks {
		if t.Slot != 0 {
			kept = "the branches stay" // the slots went back as their tasks ended
			break
		}
	}
	fmt.Fprintf(&b, "\nNext: batch_show with batch_id %q and a task to read its diff; batch_merge to land passing branches; "+
		"%s until `hrn batch rm %s`.", res.ID, kept, res.ID)
	return b.String()
}

// batchBrief is what a host model is handed as structured content. Hosts may
// show it instead of the text, so it carries the same facts and the next step,
// and leaves out what only a person debugging needs: worktree paths, the
// prompts sent back, token counts and gate output are all in the record.
type batchBrief struct {
	ID     string      `json:"batch_id"`
	Passed int         `json:"passed"`
	Total  int         `json:"total"`
	Tasks  []taskBrief `json:"tasks"`
	Next   string      `json:"next"`
}

type taskBrief struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Backend  string `json:"backend,omitempty"`
	Attempts int    `json:"attempts"`
	Branch   string `json:"branch,omitempty"`
	Commit   string `json:"commit,omitempty"`
	Changed  bool   `json:"changed"`
	Failed   string `json:"failed,omitempty"` // the gate or error that stopped it
}

func brief(res batch.Result) batchBrief {
	out := batchBrief{ID: res.ID, Passed: passed(res), Total: len(res.Tasks),
		Next: fmt.Sprintf("batch_show {batch_id: %q, task: <id>} for a diff; batch_merge {batch_id: %q} to land passing branches", res.ID, res.ID)}
	for _, t := range res.Tasks {
		tb := taskBrief{ID: t.ID, Status: string(t.Status), Attempts: len(t.Attempts), Changed: t.Commit != "", Failed: tools.OneLine(t.Error, 200)}
		if t.Commit != "" {
			tb.Branch, tb.Commit = t.Branch, tools.ShortSHA(t.Commit)
		}
		if n := len(t.Attempts); n > 0 {
			last := t.Attempts[n-1]
			tb.Backend = last.Backend
			if g := failedGateOf(last); g != nil {
				tb.Failed = fmt.Sprintf("gate `%s` exited %d", tools.OneLine(g.Command, 80), g.ExitCode)
			} else if t.Status != batch.StatusPass && last.Result.Error != "" {
				tb.Failed = tools.OneLine(last.Result.Error, 200)
			}
		}
		out.Tasks = append(out.Tasks, tb)
	}
	return out
}

func failedGateOf(att batch.Attempt) *batch.GateResult {
	for i := range att.Gates {
		if att.Gates[i].ExitCode != 0 {
			return &att.Gates[i]
		}
	}
	return nil
}

func batchShowTool() mcp.Tool {
	return mcp.Tool{
		Name:  "batch_show",
		Title: "Show a batch",
		Description: "Without batch_id: the most recent batches. With batch_id: one line per task. " +
			"With batch_id and task: that task's diff against the commit the batch started from, " +
			"and any untracked files the diff cannot show.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"batch_id": str("A batch id, as the batch tool returned it."),
				"task":     str("A task id within the batch."),
				"limit":    num("How many batches to list without batch_id (default 10)."),
			},
		},
		Annotations: &mcp.Annotations{ReadOnlyHint: mcp.Bool(true)},
		Handler: func(ctx context.Context, call mcp.Call) (mcp.CallResult, error) {
			args, err := decode[struct {
				BatchID string `json:"batch_id"`
				Task    string `json:"task"`
				Limit   int    `json:"limit"`
			}](call.Arguments)
			if err != nil {
				return mcp.CallResult{}, err
			}
			if args.BatchID == "" {
				if args.Task != "" {
					return mcp.CallResult{}, errors.New("task needs a batch_id")
				}
				return listBatches(args.Limit)
			}
			res, err := batch.LoadResult(args.BatchID)
			if err != nil {
				return mcp.CallResult{}, err
			}
			if args.Task == "" {
				return text(batchSummary(res), brief(res), false), nil
			}
			diff, untracked, err := batch.Diff(ctx, res, args.Task)
			if err != nil {
				return mcp.CallResult{}, err
			}
			body := diff
			if strings.TrimSpace(diff) == "" {
				body = "(no changes in the diff)"
			}
			if len(untracked) > 0 {
				body += "\n\nuntracked in the worktree, not in the diff: " + strings.Join(untracked, ", ")
			}
			return text(body, map[string]any{"batch_id": res.ID, "task": args.Task, "untracked": untracked}, false), nil
		},
	}
}

func listBatches(limit int) (mcp.CallResult, error) {
	if limit <= 0 {
		limit = 10
	}
	all, unreadable, err := batch.List()
	if err != nil {
		return mcp.CallResult{}, err
	}
	if len(all) > limit {
		all = all[:limit]
	}
	var b strings.Builder
	type row struct {
		ID      string    `json:"id"`
		Started time.Time `json:"started"`
		Dir     string    `json:"dir"`
		Passed  int       `json:"passed"`
		Tasks   int       `json:"tasks"`
	}
	rows := make([]row, 0, len(all))
	for _, res := range all {
		r := row{res.ID, res.Started, res.Dir, passed(res), len(res.Tasks)}
		rows = append(rows, r)
		fmt.Fprintf(&b, "%s  %d/%d passed  %s  %s\n", r.ID, r.Passed, r.Tasks, r.Started.Format("2006-01-02 15:04"), r.Dir)
	}
	if len(rows) == 0 {
		b.WriteString("(no batches)\n")
	}
	if unreadable > 0 {
		fmt.Fprintf(&b, "(%d batch record(s) could not be read)\n", unreadable)
	}
	return text(b.String(), map[string]any{"batches": rows}, false), nil
}

func batchMergeTool() mcp.Tool {
	return mcp.Tool{
		Name:  "batch_merge",
		Title: "Merge a batch's branches",
		Description: "Merge the branches of a batch's passing tasks into the repository's current branch, in task order " +
			"(or only the tasks named). Stops at the first conflict and puts the tree back as it was before that merge. " +
			"The result names the commit from before this call for undoing merged tasks.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"batch_id": str("A batch id, as the batch tool returned it."),
				"tasks":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Only these task ids. Default: every passing task."},
				"squash":   map[string]any{"type": "boolean", "description": "One commit per task instead of a merge commit."},
			},
			"required": []any{"batch_id"},
		},
		Annotations: &mcp.Annotations{ReadOnlyHint: mcp.Bool(false)},
		Handler: func(ctx context.Context, call mcp.Call) (mcp.CallResult, error) {
			args, err := decode[struct {
				BatchID string   `json:"batch_id"`
				Tasks   []string `json:"tasks"`
				Squash  bool     `json:"squash"`
			}](call.Arguments)
			if err != nil {
				return mcp.CallResult{}, err
			}
			if args.BatchID == "" {
				return mcp.CallResult{}, errors.New("batch_id is required")
			}
			res, err := batch.LoadResult(args.BatchID)
			if err != nil {
				return mcp.CallResult{}, err
			}
			before, err := batch.Head(ctx, res.Dir)
			if err != nil {
				return mcp.CallResult{}, err
			}
			merged, mergeErr := batch.Merge(ctx, res, args.Tasks, args.Squash)
			var b strings.Builder
			done := 0
			for _, m := range merged {
				switch {
				case m.Conflict:
					fmt.Fprintf(&b, "%s: conflict merging %s; the tree is back as it was, merge it by hand with `git merge %s`\n", m.Task, m.Branch, m.Branch)
				case m.Error != "":
					fmt.Fprintf(&b, "%s: could not merge %s: %s\n", m.Task, m.Branch, tools.OneLine(m.Error, 200))
				case m.Skipped != "":
					fmt.Fprintf(&b, "%s: skipped, %s\n", m.Task, m.Skipped)
				default:
					done++
					fmt.Fprintf(&b, "%s: merged %s as %s\n", m.Task, m.Branch, tools.ShortSHA(m.Commit))
				}
			}
			fmt.Fprintf(&b, "\n%d branch(es) merged into %s", done, res.Dir)
			if done > 0 {
				fmt.Fprintf(&b, "\nundo with: git reset --hard %s", before)
			}
			if mergeErr != nil {
				fmt.Fprintf(&b, "\nerror: %v", mergeErr)
			}
			return text(b.String(), map[string]any{"batch_id": res.ID, "merged": merged}, mergeErr != nil), nil
		},
	}
}
