package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/tools"
)

// cmdBatch dispatches the batch subcommands. Anything that is not one of them
// is the spec file of a new batch.
func cmdBatch(args []string) (int, error) {
	if len(args) > 0 {
		switch args[0] {
		case "rm":
			return cmdBatchRemove(args[1:])
		case "show":
			return cmdBatchShow(args[1:])
		case "diff":
			return cmdBatchDiff(args[1:])
		case "merge":
			return cmdBatchMerge(args[1:])
		}
	}
	return cmdBatchRun(args)
}

// loadBatch turns the arguments of a subcommand into a record. A wrong number
// of arguments is the caller's mistake (2); a record that will not load is a
// run-time failure (1).
func loadBatch(args []string, want int, form string) (batch.Result, int, error) {
	if len(args) != want {
		return batch.Result{}, 2, fmt.Errorf("usage: hrn batch %s", form)
	}
	res, err := batch.LoadResult(args[0])
	if err != nil {
		return batch.Result{}, 1, err
	}
	return res, 0, nil
}

func cmdBatchRemove(args []string) (int, error) {
	res, code, err := loadBatch(args, 1, "rm <batch-id>")
	if err != nil {
		return code, err
	}
	if err := batch.Remove(res); err != nil {
		return 1, err
	}
	fmt.Printf("removed the %s of batch %s (%d tasks)\n", removedWhat(res), res.ID, len(res.Tasks))
	return 0, nil
}

// removedWhat is what hrn batch rm removes of a batch: a slot stays, so of a
// slot batch only the branches go.
func removedWhat(res batch.Result) string {
	for _, t := range res.Tasks {
		if t.Slot == 0 && t.Worktree != "" {
			return "worktrees and branches"
		}
	}
	return "branches"
}

func cmdBatchShow(args []string) (int, error) {
	fs := flag.NewFlagSet("batch show", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return 2, nil
	}
	res, code, err := loadBatch(fs.Args(), 1, "show [--json] <batch-id>")
	if err != nil {
		return code, err
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return 0, enc.Encode(res)
	}
	printBatch(newRenderer(false), res)
	return 0, nil
}

// cmdBatchDiff writes one task's changes to stdout, so the output can be piped
// into git apply or a pager. Anything else goes to stderr.
func cmdBatchDiff(args []string) (int, error) {
	fs := flag.NewFlagSet("batch diff", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return 2, nil
	}
	res, code, err := loadBatch(fs.Args(), 2, "diff <batch-id> <task-id>")
	if err != nil {
		return code, err
	}
	task := fs.Arg(1)
	diff, untracked, err := batch.Diff(context.Background(), res, task)
	if err != nil {
		return 1, err
	}
	if strings.TrimSpace(diff) == "" && len(untracked) == 0 {
		tr, _ := res.Find(task)
		fmt.Fprintln(os.Stderr, diffNothing(tr))
		return 0, nil
	}
	if diff != "" {
		// Printed as git wrote it: this is a patch, not a message.
		fmt.Print(diff)
	}
	if len(untracked) > 0 {
		// A diff cannot show a file git has never seen, and the agent's new
		// files are exactly that until the task commits.
		fmt.Fprintf(os.Stderr, "untracked in the worktree, not in the diff: %s\n", strings.Join(untracked, ", "))
	}
	return 0, nil
}

func cmdBatchMerge(args []string) (int, error) {
	fs := flag.NewFlagSet("batch merge", flag.ContinueOnError)
	squash := fs.Bool("squash", false, "")
	rm := fs.Bool("rm", false, "")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return 2, nil
	}
	if fs.NArg() < 1 {
		return 2, errors.New("usage: hrn batch merge [--squash] [--rm] <batch-id> [task-id...]")
	}
	res, err := batch.LoadResult(fs.Arg(0))
	if err != nil {
		return 1, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	before, err := batch.Head(ctx, res.Dir)
	if err != nil {
		return 1, err
	}

	merged, mergeErr := batch.Merge(ctx, res, fs.Args()[1:], *squash)
	r := newRenderer(false)
	done := 0
	var removeIDs []string
	for _, m := range merged {
		switch {
		case m.Conflict:
			r.Line(r.c(red, fmt.Sprintf("  %-16s conflict merging %s", m.Task, m.Branch)))
		case m.Error != "":
			r.Line(r.c(red, fmt.Sprintf("  %-16s could not merge %s", m.Task, m.Branch)))
		case m.Skipped != "":
			r.Line(r.c(dim, fmt.Sprintf("  %-16s skipped: %s", m.Task, m.Skipped)))
		default:
			done++
			if *rm && m.Commit != "" {
				removeIDs = append(removeIDs, m.Task)
			}
			r.Line(fmt.Sprintf("  %-16s merged %s as %s", m.Task, m.Branch, tools.ShortSHA(m.Commit)))
		}
	}
	var removeErr error
	removedCount := 0
	if len(removeIDs) > 0 {
		removedIDs, err := batch.RemoveTasks(res, removeIDs)
		removeErr = err
		removedCount = len(removedIDs)
		if len(removedIDs) > 0 {
			removedTasks := make([]batch.TaskResult, 0, len(removedIDs))
			for _, id := range removedIDs {
				tr, _ := res.Find(id)
				removedTasks = append(removedTasks, tr)
			}
			r.Line(r.c(dim, fmt.Sprintf("removed the %s of %d merged task(s)",
				removedWhat(batch.Result{Tasks: removedTasks}), len(removedIDs))))
		}
	}
	if done > 0 {
		if *rm {
			hint := fmt.Sprintf("undo with: git reset --hard %s", before)
			if removedCount > 0 {
				hint += "; after removal, the tasks' work is only in the reflog"
			}
			r.Line(r.c(dim, hint))
		} else {
			r.Line(r.c(dim, fmt.Sprintf("%d branch(es) merged into %s · undo with: git reset --hard %s · "+
				"remove the %s with: hrn batch rm %s", done, res.Dir, before, removedWhat(res), res.ID)))
		}
	}
	if mergeErr != nil {
		if branch := mergeConflictBranch(merged); branch != "" {
			r.Line(r.c(dim, fmt.Sprintf("the tree is back as it was; %d branch(es) merged before this one, "+
				"and this one is still there to merge by hand: git merge %s", done, branch)))
		}
		return 1, errors.Join(mergeErr, removeErr)
	}
	if removeErr != nil {
		return 1, removeErr
	}
	return 0, nil
}

// diffNothing says why a task's diff is empty. A slot task's diff is its
// commit alone, so without one it committed nothing. A kept slot says only
// that giving it back failed (work left in it, or a busy lock), so it is
// named for inspection, not said to hold work.
func diffNothing(tr batch.TaskResult) string {
	if tr.Slot == 0 {
		return fmt.Sprintf("task %s changed nothing", tr.ID)
	}
	text := fmt.Sprintf("task %s committed nothing", tr.ID)
	if strings.Contains(tr.Error, fmt.Sprintf("slot %d kept", tr.Slot)) {
		text += fmt.Sprintf("; slot %d was kept: inspect it (hrn ws list)", tr.Slot)
	}
	return text
}

// mergeConflictBranch names the branch that stopped a merge run.
func mergeConflictBranch(merged []batch.MergeResult) string {
	for _, m := range merged {
		if m.Conflict {
			return m.Branch
		}
	}
	return ""
}

func cmdBatchRun(args []string) (int, error) {
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
		return 2, errors.New("usage: hrn batch [flags] <tasks.json>")
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	r := newRenderer(false)
	if *jsonOut {
		r.out = os.Stderr
	}
	res, err := batch.Run(ctx, spec, func(e batch.Event) {
		if e.Kind == "tool" && !verbose {
			return
		}
		r.Line(printable(fmt.Sprintf("[%s] %s", e.Task, e.Text)))
	})
	if err != nil && len(res.Tasks) == 0 {
		return 1, err
	}
	if err != nil {
		// The batch ran; only its record could not be written.
		r.Line(r.c(red, "error: "+err.Error()))
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	} else {
		r.Line("")
		printBatch(r, res)
	}
	if passed(res) != len(res.Tasks) {
		return 1, nil
	}
	return 0, nil
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

// printBatch is the end-of-run table, which `hrn batch show` reprints later or
// while the batch is still going.
func printBatch(r *renderer, res batch.Result) {
	running := 0
	for _, t := range res.Tasks {
		last := "-"
		if n := len(t.Attempts); n > 0 {
			last = t.Attempts[n-1].Backend
		}
		status := string(t.Status)
		switch t.Status {
		case batch.StatusPass:
		case batch.StatusRunning:
			running++
		default:
			status = r.c(red, status)
		}
		took := t.Duration.Round(time.Second).String()
		if t.Status == batch.StatusRunning {
			took = "-" // the record holds no start time per task
		}
		declared := t.Purpose
		if declared == "" {
			declared = "-"
		}
		line := fmt.Sprintf("  %-16s %-9s %-9s %-9s %d attempt(s)  %-24s %s",
			t.ID, status, declared, last, len(t.Attempts), t.Branch, took)
		if t.AgentCommitted() {
			line += r.c(dim, " · agent committed")
		}
		r.Line(line)
	}
	// A batch still going has no duration in its record; say how long it has
	// been going instead, which is what the reader wants to know.
	tail, took := "", res.Duration.Round(time.Second).String()
	if running > 0 {
		tail = fmt.Sprintf(" · %d still running", running)
		took = time.Since(res.Started).Round(time.Second).String() + " so far"
	}
	r.Line(r.c(dim, fmt.Sprintf("── batch %s · %d/%d passed%s · %s ──",
		res.ID, passed(res), len(res.Tasks), tail, took)))
	for _, t := range res.Tasks {
		if t.Slot != 0 {
			r.Line(r.c(dim, fmt.Sprintf("each task's work is on its branch; the slots under %s go back as tasks end · "+
				"merge them with: hrn batch merge %s · delete the branches with: hrn batch rm %s",
				filepath.Dir(t.Worktree), res.ID, res.ID)))
			return
		}
	}
	r.Line(r.c(dim, fmt.Sprintf("worktrees under %s · merge them with: hrn batch merge %s · remove them with: hrn batch rm %s",
		filepath.Join(res.Dir, ".hrn", "worktrees"), res.ID, res.ID)))
}
