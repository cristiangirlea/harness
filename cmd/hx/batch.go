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
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
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

// load reads a batch record named by exactly one argument.
func load(args []string, what string) (batch.Result, error) {
	if len(args) != 1 {
		return batch.Result{}, fmt.Errorf("usage: hx batch %s", what)
	}
	return batch.LoadResult(args[0])
}

func cmdBatchRemove(args []string) (int, error) {
	res, err := load(args, "rm <batch-id>")
	if err != nil {
		return 2, err
	}
	if err := batch.Remove(res); err != nil {
		return 1, err
	}
	fmt.Printf("removed the worktrees and branches of batch %s (%d tasks)\n", res.ID, len(res.Tasks))
	return 0, nil
}

func cmdBatchShow(args []string) (int, error) {
	fs := flag.NewFlagSet("batch show", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return 2, nil
	}
	res, err := load(fs.Args(), "show [--json] <batch-id>")
	if err != nil {
		return 2, err
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
	if len(args) != 2 {
		return 2, errors.New("usage: hx batch diff <batch-id> <task-id>")
	}
	res, err := batch.LoadResult(args[0])
	if err != nil {
		return 2, err
	}
	diff, untracked, err := batch.Diff(context.Background(), res, args[1])
	if err != nil {
		return 1, err
	}
	if diff == "" && len(untracked) == 0 {
		fmt.Fprintf(os.Stderr, "task %s changed nothing\n", args[1])
		return 0, nil
	}
	if diff != "" {
		fmt.Println(diff)
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
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return 2, nil
	}
	if fs.NArg() < 1 {
		return 2, errors.New("usage: hx batch merge [--squash] <batch-id> [task-id...]")
	}
	res, err := batch.LoadResult(fs.Arg(0))
	if err != nil {
		return 2, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	merged, mergeErr := batch.Merge(ctx, res, fs.Args()[1:], *squash)
	r := newRenderer(false)
	done := 0
	for _, m := range merged {
		switch {
		case m.Conflict:
			r.Line(r.c(red, fmt.Sprintf("  %-16s conflict merging %s", m.Task, m.Branch)))
		case m.Skipped != "":
			r.Line(r.c(dim, fmt.Sprintf("  %-16s skipped: %s", m.Task, m.Skipped)))
		default:
			done++
			r.Line(fmt.Sprintf("  %-16s merged %s as %s", m.Task, m.Branch, short(m.Commit)))
		}
	}
	if mergeErr != nil {
		if done > 0 {
			r.Line(r.c(dim, fmt.Sprintf("%d branch(es) merged before this; the tree is back as it was, so "+
				"resolve it by hand with: git merge %s", done, mergeConflictBranch(merged))))
		}
		return 1, mergeErr
	}
	if done > 0 {
		r.Line(r.c(dim, fmt.Sprintf("%d branch(es) merged into %s · undo with: git reset --hard ORIG_HEAD · "+
			"remove the worktrees with: hx batch rm %s", done, res.Dir, res.ID)))
	}
	return 0, nil
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

// printBatch is the end-of-run table, which `hx batch show` reprints later or
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
		r.Line(fmt.Sprintf("  %-16s %-9s %-9s %d attempt(s)  %-24s %s",
			t.ID, status, last, len(t.Attempts), t.Branch, t.Duration.Round(time.Second)))
	}
	tail := ""
	if running > 0 {
		tail = fmt.Sprintf(" · %d still running", running)
	}
	r.Line(r.c(dim, fmt.Sprintf("── batch %s · %d/%d passed%s · %s ──",
		res.ID, passed(res), len(res.Tasks), tail, res.Duration.Round(time.Second))))
	r.Line(r.c(dim, fmt.Sprintf("worktrees under %s · merge them with: hx batch merge %s · remove them with: hx batch rm %s",
		filepath.Join(res.Dir, ".hx", "worktrees"), res.ID, res.ID)))
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
