package batch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/flow"
	"github.com/cristiangirlea/harness/internal/followup"
	"github.com/cristiangirlea/harness/internal/kernel"
	"github.com/cristiangirlea/harness/internal/remote"
	"github.com/cristiangirlea/harness/internal/resolve"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/tools"
	"github.com/cristiangirlea/harness/internal/ws"
)

type attemptPlan struct {
	routedBy *runlog.RoutedBy
	number   int    // 1-based
	prompt   string // what the agent is sent, failure context included
	commit   string // the commit subject for whatever it leaves behind
	base     string // the commit the worktree started from
	branch   string // the task's branch, which a merge takes
	// Keep work for in-place repair on retry.
	continued               bool
	gateTimeout             time.Duration
	switchedFrom, continues string
	// Model and effort are backend-scoped.
	chosenFor string
	reason    string
}

const (
	limitContext = "\n\n---\n" +
		"A previous attempt at this task (attempt %d on %s) stopped before it finished: %[2]s\n" +
		"reached its plan's usage limit. Its changes so far are committed on the current branch\n" +
		"in this workspace. Carry on from where it stopped rather than starting over.\n"
	limitLastMessage   = "\nIts last message:\n%s\n%s\n%s\n"
	gateFailureContext = "\n\n---\n" +
		"A previous attempt at this task (attempt %d on %s) did not pass its gate. Its changes\n" +
		"are already applied in this workspace and committed on the current branch. Build on\n" +
		"them rather than starting over.\n\n" +
		"The gate command `%s` %s. Output (tail):\n" +
		"%s\n%s\n%s\n"
	// Timeouts do not prove code failure; tell retries not to invent one.
	gateTimeoutContext = "\nThe gate did not fail: hrn killed it when its time limit of %s ran out, so its\n" +
		"output above is incomplete; a failure it shows from before the kill still needs fixing,\n" +
		"but do not search it for one that is not there. The command must finish within that\n" +
		"limit; make it faster, or leave it and say so in your answer, since only the operator\n" +
		"can raise \"gate_timeout\" in the batch spec.\n"
	runFailureContext = "\n\n---\n" +
		"A previous attempt on %s ended with %s: %s\n" +
		"This workspace is a fresh copy; nothing from that attempt is in it.\n"
	attemptHistory = "\nEarlier attempts, so they are not tried again: %s.\n"
)

func failedGate(att Attempt) *GateResult {
	for i := range att.Gates {
		if att.Gates[i].ExitCode != 0 {
			return &att.Gates[i]
		}
	}
	return nil
}

// Include all attempts to avoid repeating failed repairs.
func retryContext(attempts []Attempt) (text string, continued bool) {
	last := attempts[len(attempts)-1]
	switch g := failedGate(last); {
	case last.Result.Status == agent.StatusOK && g != nil:
		f := fence(g.Output)
		text = fmt.Sprintf(gateFailureContext, len(attempts), last.Backend, g.Command, g.Brief(), f, g.Output, f)
		if g.Killed == KilledTimeout {
			text += fmt.Sprintf(gateTimeoutContext, g.Timeout)
		}
		continued = true
	case last.Result.Status == agent.StatusLimited:
		text = fmt.Sprintf(limitContext, len(attempts), last.Backend)
		if final := strings.TrimSpace(last.Result.Final); final != "" {
			f := fence(final)
			text += fmt.Sprintf(limitLastMessage, f, tail(final, 4000), f)
		}
		continued = true
	default:
		text = fmt.Sprintf(runFailureContext, last.Backend, last.Result.Status, last.Result.Error)
	}
	if earlier := attempts[:len(attempts)-1]; len(earlier) > 0 {
		summaries := make([]string, 0, len(earlier))
		for i, att := range earlier {
			summaries = append(summaries, fmt.Sprintf("%d on %s %s", i+1, att.Backend, outcomeOf(att)))
		}
		text += fmt.Sprintf(attemptHistory, strings.Join(summaries, "; "))
	}
	return text, continued
}

func outcomeOf(att Attempt) string {
	if g := failedGate(att); g != nil {
		return fmt.Sprintf("(gate `%s` %s)", tools.OneLine(g.Command, 60), g.Brief())
	}
	if att.Result.Status != agent.StatusOK {
		return fmt.Sprintf("(%s: %s)", att.Result.Status, tools.OneLine(att.Result.Error, 80))
	}
	return "(passed)"
}

// Output must not close its enclosing Markdown fence.
func fence(s string) string {
	f := "```"
	for strings.Contains(s, f) {
		f += "`"
	}
	return f
}

type taskRun struct {
	ctx        context.Context
	spec       *Spec
	batchID    string
	kept       *keptSlots
	t          Task
	tr         TaskResult
	root, base string
	fw         *flow.Writer
	notify     func(Event)
	progress   func(TaskResult) error
	started    time.Time
	slot       *ws.Taken
	slots      bool
	decisions  int
}

type taskEnd struct {
	status Status
	text   string
}

type taskRoute struct {
	opts        agent.Options
	routedBy    *runlog.RoutedBy
	backends    []string
	gates       []string
	guard       protection
	gateTimeout time.Duration
}

func runTask(ctx context.Context, spec *Spec, batchID string, kept *keptSlots, t Task, tr TaskResult, root, base string, fw *flow.Writer, notify func(Event), progress func(TaskResult) error) TaskResult {
	s := &taskRun{ctx: ctx, spec: spec, batchID: batchID, kept: kept, t: t, tr: tr, root: root, base: base, fw: fw, notify: notify, progress: progress}
	if !s.setup() {
		return s.finish(taskEnd{StatusCanceled, "canceled"})
	}
	route, err := s.resolveRoute()
	if err != nil {
		return s.finish(s.fail(err))
	}
	return s.finish(s.attempts(route))
}

func (s *taskRun) setup() bool {
	s.started = time.Now()
	s.tr.Status, s.tr.Attempts, s.tr.Commit, s.tr.Error, s.tr.BranchMade = "", []Attempt{}, "", "", false
	s.slots = s.spec.Worktrees == WorktreesSlots
	if s.ctx.Err() != nil {
		s.fw.Emit(flow.Event{Step: TaskStep(s.t.ID), Parent: BatchStep, Kind: flow.KindTask, Task: s.t.ID, Status: flow.StatusStarted,
			Name: s.t.ID, Text: promptLine(s.t.Prompt), Reason: purposeReason(s.t.Purpose)})
		return false
	}
	s.fw.Emit(flow.Event{Step: TaskStep(s.t.ID), Parent: BatchStep, Kind: flow.KindTask, Task: s.t.ID, Status: flow.StatusStarted,
		Name: s.t.ID, Text: promptLine(s.t.Prompt), Reason: purposeReason(s.t.Purpose)})
	return true
}

func (s *taskRun) decide(st flow.Status, reason string) {
	s.decisions++
	s.fw.Emit(flow.Event{Step: fmt.Sprintf("%s/d%d", TaskStep(s.t.ID), s.decisions), Parent: TaskStep(s.t.ID), Kind: flow.KindDecision,
		Task: s.t.ID, Status: st, Reason: reason})
}

func (s *taskRun) resolveRoute() (taskRoute, error) {
	opts := s.t.Options.Over(s.spec.Options)
	backend := firstOf(s.t.Backend, s.spec.Backend)
	routedBy, routeNote, err := resolve.RouteWithNote(&opts, s.t.Purpose, s.t.Prompt, &backend, runlog.Home())
	if err != nil {
		return taskRoute{}, err
	}
	routeReason := routeNote
	if backend == "" {
		routeReason = agent.WithDefault(routeNote, resolve.DefaultBackendNameIn(runlog.Home()))
	}
	if routedBy != nil {
		mode := "classified"
		if routedBy.Declared {
			mode = "declared"
		}
		routeReason = fmt.Sprintf("route category %s, %s, set by %s", routedBy.Category, mode, routedBy.Set)
		if routedBy.Eval != "" {
			routeReason += ", from eval " + routedBy.Eval
		}
	}
	s.fw.Emit(flow.Event{Step: TaskStep(s.t.ID) + "/route", Parent: TaskStep(s.t.ID), Kind: flow.KindRoute, Task: s.t.ID,
		Status: flow.StatusOK, Name: firstOf(backend, resolve.DefaultBackendNameIn(runlog.Home())), Reason: routeReason})
	backends := []string{firstOf(backend, resolve.DefaultBackendNameIn(runlog.Home()))}
	p, err := resolve.BackendPreferences(runlog.Home())
	if err != nil {
		return taskRoute{}, err
	}
	if err := p.Check(backends[0]); err != nil {
		return taskRoute{}, err
	}
	backends = append(backends, firstList(s.t.Retry, s.spec.Retry)...)
	gates := firstList(s.t.Gate, s.spec.Gate)
	guard := protection{globs: firstList(s.t.Protect, s.spec.Protect), fail: firstOf(s.t.ProtectMode, s.spec.ProtectMode) == ProtectFail, base: s.base}
	gateTimeout := defaultGateTimeout
	if d := firstNonZero(s.t.GateTimeout, s.spec.GateTimeout); d > 0 {
		gateTimeout = time.Duration(d)
	}
	if _, err := gitOut(s.ctx, s.root, "rev-parse", "--verify", "--quiet", "refs/heads/"+s.tr.Branch); err == nil {
		return taskRoute{}, errors.New(takenError("branch", s.tr.Branch))
	}
	if !s.slots {
		if _, err := os.Stat(s.tr.Worktree); err == nil {
			return taskRoute{}, errors.New(takenError("worktree", s.tr.Worktree))
		}
	}

	return taskRoute{opts, routedBy, backends, gates, guard, gateTimeout}, nil
}

func (s *taskRun) attempts(route taskRoute) taskEnd {
	opts, routedBy, backends, gates, guard, gateTimeout := route.opts, route.routedBy, route.backends, route.gates, route.guard, route.gateTimeout
	task := agent.Task{Options: opts, OriginalPrompt: s.t.Prompt, Purpose: s.t.Purpose} // Dir is the worktree, once there is one
	expect, err := compileExpect(s.t.Expect)
	if err != nil {
		return s.fail(err)
	}

	// Skip exhausted backends even without a reset time.
	spent := followup.Spent{}
	var outOfPlans string // why an attempt was skipped: claude and codex both limited
	var carry *Attempt    // the limited attempt the next one carries on
	for i := 0; i < len(backends); i++ {
		name := backends[i]
		plan := attemptPlan{number: len(s.tr.Attempts) + 1, prompt: s.t.Prompt, gateTimeout: gateTimeout, chosenFor: backends[0], base: s.base, branch: s.tr.Branch}
		if i == 0 {
			plan.routedBy = routedBy
		}
		if carry != nil {
			plan.switchedFrom, plan.continues = carry.Backend, carry.RunID
			carry = nil
		}
		if !opts.NoSwitch {
			pick, why, bothOut := followup.Pick(name, time.Now(), spent)
			if bothOut {
				outOfPlans = why
				text := "skipping an attempt on " + name + ": " + why
				s.decide(flow.StatusSkipped, text)
				s.notify(Event{Task: s.t.ID, Kind: "switch", Text: text})
				continue
			}
			if pick != name && retrievalOn(opts) {
				bound := name
				if task.RetrievalState != nil && task.RetrievalState.Scope.Backend != "" {
					bound = task.RetrievalState.Scope.Backend
				}
				s.tr.Error = pinnedSwitch(name, pick, bound)
				s.decide(flow.StatusSkipped, s.tr.Error)
				return taskEnd{StatusFail, "limited: " + s.tr.Error}
			}
			if pick != name {
				plan.switchedFrom, name = name, pick
				s.decide(flow.StatusOK, why)
				s.notify(Event{Task: s.t.ID, Kind: "switch", Text: why})
			} else if why != "" {
				s.decide(flow.StatusOK, why)
				s.notify(Event{Task: s.t.ID, Kind: "info", Text: why})
			}
		}
		b, err := followup.Backend(name)
		if err != nil {
			return s.fail(err)
		}
		if err := agent.CheckCompactWindow(name, opts.CompactWindow); err != nil {
			return s.fail(err)
		}
		if len(s.tr.Attempts) > 0 {
			text, continued := retryContext(s.tr.Attempts)
			plan.prompt, plan.continued = s.t.Prompt+text, continued
			where := "fresh worktree"
			if s.slots {
				where = "slot reset to the base"
			}
			if continued {
				where = "continuing in worktree"
			}
			verb := "retrying on " + name
			if plan.continues != "" {
				verb = fmt.Sprintf("%s reached its usage limit; carrying on on %s", plan.switchedFrom, name)
			}
			text = fmt.Sprintf("%s (%s)", verb, where)
			s.decide(flow.StatusOK, text)
			s.notify(Event{Task: s.t.ID, Kind: "retry", Text: text})
		}
		plan.commit = s.t.ID + ": " + tools.OneLine(s.t.Prompt, 60)
		if plan.continued {
			plan.commit += fmt.Sprintf(" (attempt %d)", plan.number)
		}
		if !plan.continued {
			switch {
			case s.slots && s.slot != nil:
				// The last attempt left nothing uncommitted, so -C drops only its commit; ignored build output stays.
				if _, err := gitOut(s.ctx, s.slot.Path, "switch", "-q", "-C", s.tr.Branch, s.base); err != nil {
					return s.fail(err)
				}
			case s.slots:
				made := func(mine bool) error {
					cur := s.tr
					cur.BranchMade, cur.Status = mine, StatusRunning
					err := s.progress(cur) // whether the branch is this task's from now on
					if err == nil || mine {
						s.tr.BranchMade = mine
					}
					return err
				}
				got, err := takeSlotFlow(s.ctx, s.spec, s.batchID, s.kept, s.t.ID, s.tr.Branch, s.root, s.base, s.fw, s.notify, made)
				if err != nil {
					if s.tr.BranchMade {
						s.tr.Error = err.Error() // the branch is left: say so even when canceled
					}
					return s.fail(err)
				}
				s.slot = &got
				s.tr.Worktree, s.tr.Slot = got.Path, got.N
				cur := s.tr
				cur.Status = StatusRunning
				s.progress(cur) // the record names the slot from now on
			default:
				if len(s.tr.Attempts) > 0 {
					// Drop ownership before another batch can take the path.
					cur := s.tr
					cur.BranchMade, cur.Status = false, StatusRunning
					if err := s.progress(cur); err != nil {
						return s.fail(err) // the worktree stays, and is still claimed
					}
					s.tr.BranchMade = false
					if err := removeWorktree(s.root, s.tr); err != nil {
						s.tr.BranchMade, _ = stillMade(s.root, s.tr)
						return s.fail(err)
					}
				}
				if err := addWorktree(s.ctx, s.root, s.tr, s.base); err != nil {
					return s.fail(err)
				}
				cur := s.tr
				cur.BranchMade, cur.Status = true, StatusRunning
				if err := s.progress(cur); err != nil {
					// Remove now: cleanup cannot recover an unrecorded claim.
					err = fmt.Errorf("saving the claim on worktree %s: %w", s.tr.Worktree, err)
					if rerr := removeWorktree(s.root, s.tr); rerr != nil {
						var left string
						s.tr.BranchMade, left = stillMade(s.root, s.tr) // the last save may still record it
						return s.fail(fmt.Errorf("%w; %s: %v", err, left, rerr))
					}
					return s.fail(err)
				}
				s.tr.BranchMade = true // the worktree and branch are this task's from now on
			}
			s.tr.Commit = ""
		}
		task.Dir = s.tr.Worktree
		s.notify(Event{Task: s.t.ID, Kind: "start", Text: fmt.Sprintf("%s in %s", name, s.tr.Worktree)})

		// Copy slices: workers append while records are saved.
		report := func(att Attempt) {
			cur := s.tr
			cur.Status = StatusRunning
			att.Gates = slices.Clone(att.Gates)
			cur.Attempts = append(slices.Clone(s.tr.Attempts), att)
			s.progress(cur)
		}
		at := task
		if at.Lean == nil && agent.AppliesLean(b) {
			at.Lean = agent.Bool(true) // lean by default in a batch
		}
		plan.number = len(s.tr.Attempts) + 1
		plan.reason = attemptReason(append(slices.Clone(s.tr.Attempts), Attempt{Backend: name, Continued: plan.continued, SwitchedFrom: plan.switchedFrom}), len(s.tr.Attempts))
		out, err := runAttempt(s.ctx, b, s.t.ID, at, plan, expect, guard, gates, s.fw, s.notify, report, s.spec.NoTurnDefault)
		s.tr.Attempts = append(s.tr.Attempts, out.att)
		if out.keepWorktree && s.slot != nil {
			s.kept.add(s.slot.Lease.Holder)
			err = fmt.Errorf("%w; slot %d kept", err, s.slot.N)
			s.slot = nil
		}
		task.RetrievalState = out.att.Result.Retrieval
		task.RetrievalRepair = false
		for _, g := range out.att.Gates {
			if g.ExitCode != 0 {
				task.RetrievalRepair = true
			}
		}
		// Agents can reset the branch; retain old commits only on failure or missing worktrees.
		if _, gone := os.Stat(task.Dir); err == nil && (out.commit != "" || gone == nil) {
			s.tr.Commit = out.commit
		}
		switch {
		case err != nil:
			return s.fail(err)
		case out.pass:
			text := "pass · branch " + s.tr.Branch
			if s.tr.Commit != "" {
				text += " · commit " + tools.ShortSHA(s.tr.Commit)
			}
			return taskEnd{StatusPass, text}
		case s.ctx.Err() != nil:
			return taskEnd{StatusCanceled, "canceled"}
		}
		if g := failedGate(out.att); g != nil && g.InfrastructureError != "" {
			s.tr.Error = fmt.Sprintf("gate %q: %s", g.Command, g.Outcome())
			return taskEnd{StatusFail, s.tr.Error}
		}
		if att := out.att; att.Result.Status == agent.StatusLimited {
			spent[att.Backend] = true
			if err := followup.Note(att.Backend, att.Result); err != nil {
				text := "the limit was not remembered: " + err.Error()
				s.decide(flow.StatusOK, text)
				s.notify(Event{Task: s.t.ID, Kind: "switch", Text: text})
			}
			// A usage-limit carry-on preserves work without consuming a retry.
			if other := followup.Partner(att.Backend, time.Now(), spent); other != "" && !opts.NoSwitch {
				if ok, why := followup.Ready(other, time.Now(), spent); ok && retrievalOn(opts) {
					s.tr.Error = pinnedSwitch(att.Backend, other, att.Backend)
					return taskEnd{StatusFail, "limited: " + s.tr.Error}
				} else if ok {
					backends = slices.Insert(backends, i+1, other)
					carry = &att
				} else {
					text := "not carried on: " + why
					s.decide(flow.StatusOK, text)
					s.notify(Event{Task: s.t.ID, Kind: "switch", Text: text})
				}
			}
		}
	}
	if len(s.tr.Attempts) == 0 && outOfPlans != "" {
		s.tr.Error = outOfPlans
		return taskEnd{StatusFail, "not started: " + outOfPlans}
	}
	left := "worktree kept at " + s.tr.Worktree
	if s.slots {
		left = "branch " + s.tr.Branch + " kept"
	}
	return taskEnd{StatusFail, fmt.Sprintf("fail after %d attempt(s) · %s", len(s.tr.Attempts), left)}
}

func (s *taskRun) finish(end taskEnd) TaskResult {
	status, text := end.status, end.text
	if s.slot != nil {
		if err := giveBack(*s.slot, s.base); err != nil {
			s.kept.add(s.slot.Lease.Holder)
			msg := fmt.Sprintf("slot %d kept: %v", s.slot.N, err)
			s.tr.Error = strings.TrimSpace(s.tr.Error + "\n" + msg)
			text += " · " + msg
		}
		s.slot = nil
	}
	s.tr.Status = status
	s.tr.Duration = time.Since(s.started)
	st := taskStatus(status)
	e := flow.Event{Step: TaskStep(s.t.ID), Kind: flow.KindTask, Task: s.t.ID, Status: st, Text: firstLineOf(s.tr.Error)}
	if st == flow.StatusOK && s.tr.Commit != "" {
		e.Next = "review the diff with hrn batch show " + s.batchID + " " + s.t.ID + ", then merge it"
	}
	s.fw.Emit(e)
	s.notify(Event{Task: s.t.ID, Kind: "done", Text: text})
	return s.tr
}

func (s *taskRun) fail(err error) taskEnd {
	s.tr.Error = err.Error()
	if s.ctx.Err() != nil {
		return taskEnd{StatusCanceled, "canceled"}
	}
	return taskEnd{StatusError, "error: " + err.Error()}
}

func proposeAdditions(ctx context.Context, backendName string, t Task, tr TaskResult, k int, root string, opts agent.Options) ([]string, error) {
	b, err := followup.Backend(backendName)
	if err != nil {
		return nil, err
	}
	var gate string
	if len(tr.Attempts) > 0 {
		if g := failedGate(tr.Attempts[len(tr.Attempts)-1]); g != nil {
			gate = g.Command + "\n" + tail(g.Output, 4096)
		}
	}
	record, err := json.Marshal(tr)
	if err != nil {
		return nil, err
	}
	prompt := fmt.Sprintf("Task: %s\nFailing gate: %s\nRun record: %s\nPropose exactly %d distinct short brief additions as a JSON array of strings.", t.Prompt, gate, tail(string(record), 8192), k)
	opts.Mode = agent.ModeReadOnly
	r := agent.Run(ctx, b, agent.Task{Prompt: prompt, Dir: root, Purpose: "plan", Options: opts, Knowledge: remote.SearcherFromEnv}, func(agent.Event) {})
	if r.Status != agent.StatusOK {
		return nil, fmt.Errorf("proposal failed: %s", r.Error)
	}
	var additions []string
	found := false
	for i := 0; i < len(r.Final); i++ {
		if r.Final[i] != '[' {
			continue
		}
		var parsed []string
		if json.NewDecoder(strings.NewReader(r.Final[i:])).Decode(&parsed) == nil {
			additions, found = parsed, true
			break
		}
	}
	if !found {
		return nil, errors.New("proposal has no JSON string array")
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(additions))
	for _, a := range additions {
		a = strings.TrimSpace(a)
		if a != "" && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
		if len(out) == k {
			break
		}
	}
	if len(out) < 2 {
		return nil, errors.New("proposal has fewer than two distinct additions")
	}
	return out, nil
}

func runBestOfK(ctx context.Context, spec *Spec, batchID string, kept *keptSlots, t Task, tr TaskResult, root, base string, fw *flow.Writer, notify func(Event), progress func(TaskResult) error) TaskResult {
	k := t.RetryK
	if k == 0 {
		k = spec.RetryK
	}
	decision := func(id, message string, status flow.Status) {
		fw.Emit(flow.Event{Step: TaskStep(t.ID) + "/best-k-" + id, Parent: TaskStep(t.ID), Kind: flow.KindDecision, Task: t.ID, Status: status, Reason: message})
		notify(Event{Task: t.ID, Kind: "decision", Text: message})
	}
	decision("proposal", fmt.Sprintf("best-of-k: proposing %d additions", k), flow.StatusOK)
	backend := firstOf(t.Backend, spec.Backend)
	if len(tr.Attempts) > 0 {
		backend = tr.Attempts[len(tr.Attempts)-1].Backend
	}
	backend = firstOf(backend, resolve.DefaultBackendNameIn(runlog.Home()))
	additions, err := proposeAdditions(ctx, backend, t, tr, k, root, t.Options.Over(spec.Options))
	if err != nil {
		tr.Error = strings.TrimSpace(tr.Error + "\n" + err.Error())
		if ctx.Err() != nil {
			tr.Status = StatusCanceled
			decision("result", "best-of-k: canceled", flow.StatusCanceled)
		} else {
			decision("result", err.Error(), flow.StatusFailed)
		}
		return tr
	}
	results := make([]TaskResult, len(additions))
	candidateSpec := *spec
	candidateSpec.Retry = nil
	var wg sync.WaitGroup
	var candidateMu sync.Mutex
	tr.Candidates = make([]RetryCandidate, len(additions))
	for i, addition := range additions {
		tr.Candidates[i].BriefAddition = addition
	}
	for i, addition := range additions {
		wg.Add(1)
		go func(i int, addition string) {
			defer wg.Done()
			ct := t
			ct.ID = fmt.Sprintf("%s-k%d", t.ID, i+1)
			ct.Prompt = t.Prompt + "\n\n" + addition
			ct.RetryK, ct.Retry = 0, nil
			ctr := TaskResult{ID: ct.ID, Prompt: ct.Prompt, Purpose: t.Purpose, Status: StatusRunning, Branch: "hrn/" + ct.ID, Attempts: []Attempt{}}
			if spec.Worktrees == WorktreesFresh {
				ctr.Worktree = filepath.Join(root, ".hrn", "worktrees", ct.ID)
			}
			results[i] = runTask(ctx, &candidateSpec, batchID, kept, ct, ctr, root, base, fw, notify, func(cur TaskResult) error {
				candidateMu.Lock()
				defer candidateMu.Unlock()
				c := &tr.Candidates[i]
				c.Branch, c.Worktree, c.Slot, c.BranchMade = cur.Branch, cur.Worktree, cur.Slot, cur.BranchMade
				snapshot := tr
				snapshot.Status = StatusRunning
				snapshot.Candidates = slices.Clone(tr.Candidates)
				return progress(snapshot)
			})
		}(i, addition)
	}
	wg.Wait()
	eligible := make([]RetryCandidate, len(additions))
	for i, ctr := range results {
		c := tr.Candidates[i]
		for _, a := range ctr.Attempts {
			c.Tokens += int(a.Result.InputTokens + a.Result.OutputTokens)
		}
		if len(ctr.Attempts) > 0 {
			last := ctr.Attempts[len(ctr.Attempts)-1]
			c.RunID, c.Gates = last.RunID, last.Gates
		}
		tr.Candidates[i], eligible[i] = c, c
		if ctr.Status != StatusPass {
			eligible[i].Gates = append(slices.Clone(c.Gates), GateResult{ExitCode: -1})
		}
	}
	winner := pickRetryWinner(eligible)
	cleanup := func(r TaskResult) bool {
		if !r.BranchMade {
			return true
		}
		if spec.Worktrees == WorktreesSlots {
			if err := removeSlotBranch(root, r); err != nil {
				tr.Error = strings.TrimSpace(tr.Error + "\ncleanup: " + err.Error())
				return false
			}
		} else if err := removeWorktree(root, r); err != nil {
			tr.Error = strings.TrimSpace(tr.Error + "\ncleanup: " + err.Error())
			return false
		}
		return true
	}
	for i, r := range results {
		if i != winner && cleanup(r) {
			tr.Candidates[i].BranchMade = false
		}
	}
	if winner >= 0 {
		if !cleanup(tr) {
			tr.Candidates = append(tr.Candidates, RetryCandidate{Branch: tr.Branch, Worktree: tr.Worktree, Slot: tr.Slot, BranchMade: tr.BranchMade})
		}
		w := results[winner]
		tr.Candidates[winner].Winner = true
		tr.Status, tr.Commit, tr.Branch, tr.Worktree, tr.Slot, tr.BranchMade = w.Status, w.Commit, w.Branch, w.Worktree, w.Slot, w.BranchMade
		decision("result", "best-of-k: winner "+w.ID, flow.StatusOK)
	} else if ctx.Err() != nil {
		tr.Status = StatusCanceled
		decision("result", "best-of-k: canceled", flow.StatusCanceled)
	} else {
		decision("result", "best-of-k: no candidate passed", flow.StatusFailed)
	}
	_ = progress(tr)
	return tr
}

// Retrieval pins cannot follow a usage-limit switch to another backend.
func pinnedSwitch(from, to, bound string) string {
	return fmt.Sprintf("%s reached its usage limit; switch to %s skipped: the retrieval pin is bound to %s", from, to, bound)
}

type attemptOutcome struct {
	att          Attempt
	commit       string // empty when the agent changed nothing
	pass         bool   // the run finished and every gate exited 0
	keepWorktree bool   // releasing a slot would move HEAD away from the agent's work
}

// runAttempt's returned error aborts the whole task; a failed run or gate is reported in the outcome instead.
func runAttempt(ctx context.Context, b agent.Backend, id string, task agent.Task, plan attemptPlan, expect *regexp.Regexp, guard protection, gates []string, fw *flow.Writer, notify func(Event), report func(Attempt), noTurnDefault bool) (attemptOutcome, error) {
	task.Prompt = plan.prompt
	task.Knowledge = remote.SearcherFromEnv
	att := Attempt{Backend: b.Name(), Prompt: plan.prompt, Continued: plan.continued, SwitchedFrom: plan.switchedFrom}
	env, cleanup, err := batchEnv(task.Dir)
	if err != nil {
		return attemptOutcome{att: att}, err
	}
	defer func() {
		if err := cleanup(); err != nil {
			notify(Event{Task: id, Kind: "info", Text: "removing temporary folder: " + err.Error()})
		}
	}()
	task.ChildEnv = env
	if plan.switchedFrom != "" {
		task.Model = "" // the model named for the other backend means nothing here
	}
	if b.Name() != plan.chosenFor {
		task.Model, task.Effort, plan.routedBy = "", "", nil
	}

	turnDefault := !noTurnDefault && resolve.ApplyTurnDefault(&task, b.Name())
	rec, err := runlog.CreateContinuing(b.Name(), task, agent.ResolveSetup(b.Name(), task), plan.continues, plan.routedBy)
	if err != nil {
		return attemptOutcome{att: att}, fmt.Errorf("creating run record: %w", err)
	}
	att.RunID = rec.ID()
	as := AttemptStep(id, plan.number)
	fw.Emit(flow.Event{Step: as, Parent: TaskStep(id), Kind: flow.KindAttempt, Task: id, Attempt: plan.number,
		Status: flow.StatusStarted, Name: b.Name(), Reason: plan.reason, Run: att.RunID})
	task.RunDir = rec.Dir() // so a finished attempt can be continued
	if turnDefault {
		rec.Event(agent.Event{Time: time.Now(), Kind: agent.EventInfo, Text: resolve.TurnDefaultInfo})
		notify(Event{Task: id, Kind: "info", Text: resolve.TurnDefaultInfo})
	}
	report(att)
	before, beforeErr := gitTips(task.Dir, plan.branch)
	att.Result = agent.Run(ctx, b, task, func(e agent.Event) {
		rec.Event(e)
		if e.Kind == agent.EventToolCall {
			notify(Event{Task: id, Kind: "tool", Text: strings.TrimSpace("→ " + e.Tool + " " + tools.OneLine(e.Text, 120))})
		}
	})
	att.DidNotStart = startFailure(att)
	if after, err := gitTips(task.Dir, plan.branch); beforeErr == nil && err == nil && before != "" && after != before {
		att.AgentCommitted = true
	}
	if err := rec.Close(att.Result); err != nil {
		att.Error = runlog.CloseMessage(err)
	}
	text := fmt.Sprintf("%s · %d turns · run %s", att.Result.Status, att.Result.Turns, att.RunID)
	if att.Result.Error != "" {
		text += " · " + tools.OneLine(att.Result.Error, 160)
	}
	if att.Error != "" {
		text += " · " + tools.OneLine(att.Error, 160)
	}
	report(att)
	notify(Event{Task: id, Kind: "run", Text: text})

	pass := att.Result.Status == agent.StatusOK
	if pass && expect != nil {
		fw.Emit(flow.Event{Step: GateStep(id, plan.number, len(att.Gates)+1), Parent: as, Kind: flow.KindGate, Task: id, Attempt: plan.number,
			Status: flow.StatusStarted, Name: expectCommand(expect.String())})
		gr := checkExpect(expect, att.Result.Final)
		att.Gates = append(att.Gates, gr)
		att.Result.Evidence = append(att.Result.Evidence, kernel.NewEvidence("gate", gr.Command, []byte(gr.Output)))
		fw.Emit(gateEvent(id, plan.number, len(att.Gates), as, gr))
		report(att)
		if gr.ExitCode != 0 {
			pass = false
			notify(Event{Task: id, Kind: "gate", Text: "✗ " + gr.Command})
		} else {
			notify(Event{Task: id, Kind: "gate", Text: "✓ " + gr.Command})
		}
	}
	if len(guard.globs) > 0 && att.Result.Status == agent.StatusOK {
		changed, err := changedProtected(ctx, task.Dir, guard)
		att.Protected = changed
		if len(changed) > 0 {
			notify(Event{Task: id, Kind: "gate", Text: "! protected files changed: " + strings.Join(changed, ", ")})
		}
		if len(changed) > 0 && !(guard.fail && pass) {
			fw.Emit(protectedInfo(id, plan.number, as, changed))
		}
		if guard.fail && pass && (err != nil || len(changed) > 0) {
			gr := protectGate(guard, changed, err)
			att.Gates = append(att.Gates, gr)
			att.Result.Evidence = append(att.Result.Evidence, kernel.NewEvidence("gate", gr.Command, []byte(gr.Output)))
			fw.Emit(gateEvent(id, plan.number, len(att.Gates), as, gr))
			pass = false
			notify(Event{Task: id, Kind: "gate", Text: "✗ " + gr.Command})
		} else if err != nil {
			att.Error = strings.TrimSpace(att.Error + "\nprotect: " + err.Error())
		}
		report(att)
	}
	if pass {
		for _, g := range gates {
			gateN := len(att.Gates) + 1
			fw.Emit(flow.Event{Step: GateStep(id, plan.number, gateN), Parent: as, Kind: flow.KindGate, Task: id, Attempt: plan.number, Status: flow.StatusStarted, Name: g})
			stop := beating(gateBeat, func() { report(att) })
			gr := runGate(ctx, task.Dir, g, plan.gateTimeout, env)
			stop()
			att.Result.Evidence = append(att.Result.Evidence, kernel.NewEvidence("gate", gr.Command, []byte(gr.Output)))
			att.Gates = append(att.Gates, gr)
			fw.Emit(gateEvent(id, plan.number, gateN, as, gr))
			report(att)
			if gr.ExitCode != 0 {
				pass = false
				notify(Event{Task: id, Kind: "gate", Text: fmt.Sprintf("✗ %s (%s)", g, gr.Outcome())})
				break
			}
			notify(Event{Task: id, Kind: "gate", Text: "✓ " + g})
		}
	}

	// Commit failed work so worktree removal cannot lose it.
	sha, note, commitErr := commitChanges(task.Dir, plan.branch, plan.commit)
	if note != "" {
		// No retry may remove or reuse the worktree holding the work.
		att.Error = strings.TrimSpace(att.Error + "\n" + note)
		fw.Emit(flow.Event{Step: as, Parent: TaskStep(id), Kind: flow.KindAttempt, Task: id, Attempt: plan.number, Status: flow.StatusFailed, Reason: attemptEndReason(att), Text: firstLineOf(note)})
		return attemptOutcome{att: att, keepWorktree: true}, fmt.Errorf("%s (%s)", note, task.Dir)
	}
	if sha == "" && commitErr == nil {
		sha, note, commitErr = agentCommit(task.Dir, plan.branch, plan.base)
		if note != "" {
			att.Error = strings.TrimSpace(att.Error + "\n" + note)
		}
	}
	if commitErr != nil {
		att.Error = strings.TrimSpace(att.Error + "\ncommit: " + commitErr.Error())
		fw.Emit(flow.Event{Step: as, Parent: TaskStep(id), Kind: flow.KindAttempt, Task: id, Attempt: plan.number, Status: flow.StatusFailed, Reason: attemptEndReason(att), Text: firstLineOf(commitErr.Error())})
		return attemptOutcome{att: att}, fmt.Errorf("commit failed, changes are uncommitted in %s: %v", task.Dir, commitErr)
	}
	if sha != "" || note != "" {
		fw.Emit(flow.Event{Step: as + "/commit", Parent: as, Kind: flow.KindCommit, Task: id, Attempt: plan.number, Status: flow.StatusOK, Text: firstOf(sha, note)})
	}
	fw.Emit(flow.Event{Step: as, Parent: TaskStep(id), Kind: flow.KindAttempt, Task: id, Attempt: plan.number, Status: attemptStatus(att), Reason: attemptEndReason(att), Text: firstLineOf(att.Result.Error)})
	return attemptOutcome{att: att, commit: sha, pass: pass}, nil
}

// startFailure needs evidence the model was never reached: some adapters report no turns even when it was.
func startFailure(a Attempt) string {
	r := a.Result
	reached := r.Turns != 0 || r.ToolCalls != 0 || r.Final != "" || r.InputTokens != 0 || r.OutputTokens != 0 || r.ContextTokens != 0
	if reached || r.Status != agent.StatusError || r.Error == "" {
		return ""
	}
	return strings.TrimSpace(firstLineOf(r.Error))
}
