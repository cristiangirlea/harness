// Package batch runs parallel tasks in isolated git worktrees with gates and retries.
package batch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/flow"
	"github.com/cristiangirlea/harness/internal/mcp"
	"github.com/cristiangirlea/harness/internal/owner"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/ws"
)

// Run executes and records a batch, stopping agents when ctx is canceled.
// Run delivers events one at a time, so the callback need not lock.
func Run(ctx context.Context, spec Spec, notify func(Event)) (Result, error) {
	started := time.Now()
	if err := spec.normalize(); err != nil {
		return Result{}, err
	}
	report := notify
	if report == nil {
		report = func(Event) {}
	}
	var notifyMu sync.Mutex
	notify = func(e Event) {
		notifyMu.Lock()
		defer notifyMu.Unlock()
		report(e)
	}

	root, err := repoRoot(ctx, spec.Dir)
	if err != nil {
		return Result{}, err
	}
	base, err := gitOut(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return Result{}, err
	}
	if spec.Base != "" {
		if base, err = gitOut(ctx, root, "rev-parse", "--verify", "--quiet", "--end-of-options", spec.Base+"^{commit}"); err != nil {
			return Result{}, fmt.Errorf("base %s: %w", spec.Base, err)
		}
	}
	if !spec.AllowDirty {
		if err := checkClean(ctx, root); err != nil {
			return Result{}, err
		}
	}
	if spec.MCP, err = mcpPathsOf(spec.Dir, spec.MCP); err != nil {
		return Result{}, err
	}
	for i := range spec.Tasks {
		if t := &spec.Tasks[i]; t.MCP != nil {
			if t.MCP, err = mcpPathsOf(spec.Dir, t.MCP); err != nil {
				return Result{}, fmt.Errorf("task %s: %w", t.ID, err)
			}
		}
	}
	// Keep fresh worktrees out of status; ws manages slot ignores.
	wtRoot := filepath.Join(root, ".hrn", "worktrees")
	fresh := spec.Worktrees == WorktreesFresh
	if fresh {
		if err := os.MkdirAll(wtRoot, 0o755); err != nil {
			return Result{}, err
		}
		if _, err := os.Stat(filepath.Join(root, ".hrn", ".gitignore")); err != nil {
			_ = os.WriteFile(filepath.Join(root, ".hrn", ".gitignore"), []byte("*\n"), 0o644)
		}
	}

	id := runlog.NewID()
	issues, err := takeIssues(ctx, &spec, root, id, notify)
	if err != nil {
		return Result{}, err
	}
	res := Result{ID: id, Owner: owner.Local(), Started: started, Dir: root, Base: base, Parallel: spec.Parallel, Tasks: make([]TaskResult, len(spec.Tasks)), Ownership: OwnershipClaim}
	for i, t := range spec.Tasks {
		res.Tasks[i] = TaskResult{ID: t.ID, Prompt: t.Prompt, Purpose: t.Purpose, Status: StatusRunning, Branch: "hrn/" + t.ID, Attempts: []Attempt{}}
		if fresh {
			res.Tasks[i].Worktree = filepath.Join(wtRoot, t.ID)
		}
	}
	if err := save(res); err != nil {
		releaseIssues(ctx, issues, "batch "+id+" did not start", notify, spec.Tasks)
		return Result{}, fmt.Errorf("saving batch record: %w", err)
	}
	flowWriter, _ := flow.Open(FlowPath(res.ID), flow.Event{Owner: res.Owner, Batch: res.ID})
	defer func() { _ = flowWriter.Close() }()
	flowWriter.Emit(flow.Event{Step: BatchStep, Kind: flow.KindBatch, Status: flow.StatusStarted, Name: res.ID,
		Text: fmt.Sprintf("%d tasks, %d parallel, base %s", len(res.Tasks), res.Parallel, short(res.Base))})

	// Intermediate save failures stale the view; final save errors are returned.
	var recordMu sync.Mutex
	update := func(i int, tr TaskResult) error {
		recordMu.Lock()
		defer recordMu.Unlock()
		res.Tasks[i] = tr
		return save(res)
	}

	kept := &keptSlots{}
	idx := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < spec.Parallel; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range idx {
				tr := runTask(ctx, &spec, res.ID, kept, spec.Tasks[i], res.Tasks[i], root, base, flowWriter, notify,
					func(tr TaskResult) error { return update(i, tr) })
				k := spec.Tasks[i].RetryK
				if k == 0 {
					k = spec.RetryK
				}
				if k >= 2 && tr.Status == StatusFail && len(tr.Attempts) > 0 && failedGate(tr.Attempts[len(tr.Attempts)-1]) != nil && ctx.Err() == nil {
					tr = runBestOfK(ctx, &spec, res.ID, kept, spec.Tasks[i], tr, root, base, flowWriter, notify, func(cur TaskResult) error { return update(i, cur) })
				}
				update(i, tr)
				releaseIssue(ctx, issues[i], "batch "+res.ID+" task "+spec.Tasks[i].ID+" ended", notify, spec.Tasks[i].ID)
			}
		}()
	}
	for i := range spec.Tasks {
		idx <- i
	}
	close(idx)
	wg.Wait()
	res.Duration = time.Since(started)
	if err := save(res); err != nil {
		return res, fmt.Errorf("saving batch record: %w", err)
	}
	flowWriter.Emit(flow.Event{Step: BatchStep, Kind: flow.KindBatch, Status: batchStatus(res), Text: tally(res)})
	return res, nil
}

// takeIssues leases every task's issue before any task starts, so a refusal starts nothing.
func takeIssues(ctx context.Context, spec *Spec, root, id string, notify func(Event)) ([]*ws.IssueLease, error) {
	refs := make([]ws.IssueRef, len(spec.Tasks))
	owners := map[ws.IssueRef]string{}
	for i, t := range spec.Tasks {
		issue := t.Options.Over(spec.Options).Issue
		if issue == "" {
			continue
		}
		ref, err := ws.ParseIssue(issue, root)
		if err != nil {
			return nil, fmt.Errorf("task %s: %w", t.ID, err)
		}
		if other, ok := owners[ref]; ok {
			return nil, fmt.Errorf("tasks %s and %s both name issue %s; one run per issue, so give each task its own", other, t.ID, ref)
		}
		owners[ref], refs[i] = t.ID, ref
	}
	leases := make([]*ws.IssueLease, len(spec.Tasks))
	for i, t := range spec.Tasks {
		if refs[i].Repo == "" {
			continue
		}
		// Routing picks an unset backend later, so only an explicit one names the worker.
		l := ws.Lease{Holder: "hrn batch", Run: id + "/" + t.ID, Backend: firstOf(t.Backend, spec.Backend), Branch: "hrn/" + t.ID}
		il, err := ws.TakeIssue(ctx, runlog.Home(), refs[i], l, ws.IssueOptions{TakeOver: t.Options.Over(spec.Options).TakeOver, GitHub: true})
		if err != nil {
			releaseIssues(ctx, leases, "batch "+id+" did not start", notify, spec.Tasks)
			return nil, fmt.Errorf("task %s: %w", t.ID, err)
		}
		leases[i] = il
		for _, n := range il.Notes {
			notify(Event{Task: t.ID, Kind: "issue", Text: n})
		}
	}
	return leases, nil
}

func releaseIssues(ctx context.Context, leases []*ws.IssueLease, reason string, notify func(Event), tasks []Task) {
	for i, il := range leases {
		releaseIssue(ctx, il, reason, notify, tasks[i].ID)
	}
}

func releaseIssue(ctx context.Context, il *ws.IssueLease, reason string, notify func(Event), task string) {
	if il == nil {
		return
	}
	seen := len(il.Notes)
	if err := il.Release(ctx, reason); err != nil {
		notify(Event{Task: task, Kind: "issue", Text: err.Error()})
	}
	for _, n := range il.Notes[seen:] {
		notify(Event{Task: task, Kind: "issue", Text: n})
	}
}

// Validate MCP paths early; empty disables, absent inherits.
func mcpPathsOf(dir string, paths []string) ([]string, error) {
	if paths != nil && len(paths) == 0 {
		return []string{}, nil
	}
	abs := make([]string, 0, len(paths))
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		abs = append(abs, p)
	}
	return mcp.ConfigPaths(abs)
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstNonZero(ds ...agent.Duration) agent.Duration {
	for _, d := range ds {
		if d != 0 {
			return d
		}
	}
	return 0
}

func firstList(lists ...[]string) []string {
	for _, l := range lists {
		if l != nil {
			return l
		}
	}
	return nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
