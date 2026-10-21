package batch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/flow"
	"github.com/cristiangirlea/harness/internal/owner"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/tools"
)

// FlowPath is where a batch's live execution events are appended; outside batches/ so that holds only records.
func FlowPath(id string) string { return filepath.Join(runlog.Home(), "batch-flows", id+".jsonl") }

// Step ids are shared by the live writer and the derivation so both build the same tree.
const BatchStep = "batch"

func TaskStep(task string) string { return "task:" + task }

func SlotStep(task string) string { return TaskStep(task) + "/slot" }

func AttemptStep(task string, n int) string { return fmt.Sprintf("%s/a%d", TaskStep(task), n) }

func GateStep(task string, n, k int) string { return fmt.Sprintf("%s/gate%d", AttemptStep(task, n), k) }

// gateStep gives the protect check its own step, the same live and rebuilt.
func gateStep(task string, n, k int, g GateResult) string {
	if g.Guard {
		return AttemptStep(task, n) + "/protect"
	}
	return GateStep(task, n, k)
}

// protectedInfo records protected files changed under a guard that only reports them.
func protectedInfo(task string, n int, parent string, changed []string) flow.Event {
	return flow.Event{Step: AttemptStep(task, n) + "/protected", Parent: parent, Kind: flow.KindInfo, Task: task, Attempt: n,
		Status: flow.StatusOK, Name: "protected files changed", Text: strings.Join(changed, ", ")}
}

// LoadResultFor is LoadResult behind the read decision; a batch v may not read is reported as missing.
func LoadResultFor(v owner.Viewer, id string) (Result, error) {
	res, err := LoadResult(id)
	if err != nil {
		return res, err
	}
	if !v.CanRead(res.Owner) {
		return Result{}, fmt.Errorf("batch %s: %w", id, os.ErrNotExist)
	}
	return res, nil
}

// ListFor is List of the batches v may read.
func ListFor(v owner.Viewer) ([]Result, int, error) {
	all, unreadable, err := List()
	out := all[:0]
	for _, r := range all {
		if v.CanRead(r.Owner) {
			out = append(out, r)
		}
	}
	return out, unreadable, err
}

// Timeline is a batch's whole flow for v: its own events, live or rebuilt, and each attempt's run under it.
// A run v may not read, or one whose record is gone, shows as a gap rather than disappearing.
func Timeline(v owner.Viewer, id string) ([]flow.Event, error) {
	res, err := LoadResultFor(v, id)
	if err != nil {
		return nil, err
	}
	runs := map[string]runlog.Record{}
	var streams [][]flow.Event
	for _, t := range res.Tasks {
		for i, a := range t.Attempts {
			if a.RunID == "" {
				continue
			}
			parent := AttemptStep(t.ID, i+1)
			rec, err := runlog.LoadFor(v, a.RunID)
			if err != nil {
				streams = append(streams, []flow.Event{{Time: res.Started, Owner: res.Owner, Step: parent + "/run", Parent: parent,
					Kind: flow.KindError, Status: flow.StatusFailed, Run: a.RunID, Derived: true,
					Text: "the run record cannot be shown: " + missingWhy(err)}})
				continue
			}
			runs[a.RunID] = rec
			events, err := runlog.Events(a.RunID)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				events = nil
			}
			streams = append(streams, flow.FromRun(rec, events, parent))
		}
	}
	own, err := flow.Read(FlowPath(res.ID))
	if err != nil || len(own) == 0 {
		own = FlowOf(res, runs)
	}
	return flow.Renumber(append([][]flow.Event{own}, streams...)...), nil
}

func missingWhy(err error) string {
	if errors.Is(err, os.ErrNotExist) {
		return "it was pruned or is not yours"
	}
	return "it could not be read"
}

// FlowOf rebuilds a batch's steps from its record, for batches written before live events.
// Times not in the record come from the attempt's run, else the batch start.
func FlowOf(res Result, runs map[string]runlog.Record) []flow.Event {
	at := func(e flow.Event) flow.Event {
		e.Owner, e.Batch, e.Derived = res.Owner, res.ID, true
		return flow.Clean(e)
	}
	out := []flow.Event{at(flow.Event{Time: res.Started, Step: BatchStep, Kind: flow.KindBatch, Status: flow.StatusStarted,
		Name: res.ID, Text: fmt.Sprintf("%d tasks, %d at once, from %s", len(res.Tasks), res.Parallel, short(res.Base))})}
	for _, t := range res.Tasks {
		ts := TaskStep(t.ID)
		out = append(out, at(flow.Event{Time: res.Started, Step: ts, Parent: BatchStep, Kind: flow.KindTask, Task: t.ID,
			Status: flow.StatusStarted, Name: t.ID, Text: promptLine(t.Prompt), Reason: purposeReason(t.Purpose)}))
		for i, a := range t.Attempts {
			n := i + 1
			as := AttemptStep(t.ID, n)
			start := res.Started
			if r, ok := runs[a.RunID]; ok {
				start = r.Started
			}
			out = append(out, at(flow.Event{Time: start, Step: as, Parent: ts, Kind: flow.KindAttempt, Task: t.ID, Attempt: n,
				Run: a.RunID, Status: flow.StatusStarted, Name: a.Backend, Reason: attemptReason(t.Attempts, i)}))
			end := start.Add(a.Result.Duration)
			for k, g := range a.Gates {
				out = append(out, at(flow.Event{Time: end, Step: gateStep(t.ID, n, k+1, g), Parent: as, Kind: flow.KindGate,
					Task: t.ID, Attempt: n, Status: flow.StatusStarted, Name: g.Command}))
				end = end.Add(g.Duration)
				st, why := flow.StatusOK, ""
				if g.ExitCode != 0 {
					st, why = flow.StatusFailed, g.Outcome()
					if g.Killed == KilledCanceled {
						st = flow.StatusCanceled
					}
				}
				out = append(out, at(flow.Event{Time: end, Step: gateStep(t.ID, n, k+1, g), Parent: as, Kind: flow.KindGate,
					Task: t.ID, Attempt: n, Status: st, Name: g.Command, Reason: why, Text: lastLine(g.Output)}))
			}
			if len(a.Protected) > 0 && !hasProtectGate(a.Gates) {
				e := protectedInfo(t.ID, n, as, a.Protected)
				e.Time = end
				out = append(out, at(e))
			}
			st := attemptStatus(a)
			if st.Done() {
				out = append(out, at(flow.Event{Time: end, Step: as, Kind: flow.KindAttempt, Status: st,
					Text: strings.TrimSpace(firstLineOf(a.Result.Error) + " " + firstLineOf(a.Error))}))
			}
		}
		if st := taskStatus(t.Status); st.Done() {
			e := flow.Event{Time: res.Started.Add(t.Duration), Step: ts, Kind: flow.KindTask, Status: st, Text: firstLineOf(t.Error)}
			if st == flow.StatusOK && t.Commit != "" {
				e.Next = "review the diff with hrn batch show " + res.ID + " " + t.ID + ", then merge it"
			}
			out = append(out, at(e))
		} else if st == flow.StatusStarted && len(t.Attempts) == 0 {
			out = append(out, at(flow.Event{Time: res.Started, Step: ts, Kind: flow.KindTask, Status: flow.StatusWaiting,
				Next: "waits for a free worktree slot"}))
		}
	}
	if res.Duration > 0 {
		out = append(out, at(flow.Event{Time: res.Started.Add(res.Duration), Step: BatchStep, Kind: flow.KindBatch,
			Status: batchStatus(res), Text: tally(res)}))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

func hasProtectGate(gates []GateResult) bool {
	for _, g := range gates {
		if g.Guard {
			return true
		}
	}
	return false
}

func purposeReason(p string) string {
	if p == "" {
		return ""
	}
	return "declared purpose " + p
}

// attemptReason says why attempt i ran, from what the record holds about the one before.
func attemptReason(atts []Attempt, i int) string {
	a := atts[i]
	switch {
	case a.SwitchedFrom != "":
		return "carried on from " + a.SwitchedFrom + " after it reached its usage limit"
	case i == 0:
		return "first attempt"
	}
	prev := atts[i-1]
	why := string(prev.Result.Status)
	for _, g := range prev.Gates {
		if g.ExitCode != 0 {
			why = "gate " + g.Command + " " + g.Brief()
		}
	}
	verb := "retry on " + a.Backend
	if a.Continued {
		verb = "retry continuing the same session"
	}
	return fmt.Sprintf("%s because attempt %d ended: %s", verb, i, why)
}

func attemptStatus(a Attempt) flow.Status {
	switch a.Result.Status {
	case "":
		return flow.StatusStarted
	case agent.StatusCanceled:
		return flow.StatusCanceled
	case agent.StatusOK:
	default:
		return flow.StatusFailed
	}
	for _, g := range a.Gates {
		if g.ExitCode != 0 {
			return flow.StatusFailed
		}
	}
	return flow.StatusOK
}

func taskStatus(s Status) flow.Status {
	switch s {
	case StatusPass:
		return flow.StatusOK
	case StatusFail, StatusError:
		return flow.StatusFailed
	case StatusCanceled:
		return flow.StatusCanceled
	}
	return flow.StatusStarted
}

func batchStatus(res Result) flow.Status {
	st := flow.StatusOK
	for _, t := range res.Tasks {
		switch taskStatus(t.Status) {
		case flow.StatusCanceled:
			return flow.StatusCanceled
		case flow.StatusFailed:
			st = flow.StatusFailed
		}
	}
	return st
}

func tally(res Result) string {
	n := map[Status]int{}
	for _, t := range res.Tasks {
		n[t.Status]++
	}
	return fmt.Sprintf("%d passed, %d failed, %d errors, %d canceled in %s", n[StatusPass], n[StatusFail], n[StatusError],
		n[StatusCanceled], res.Duration.Round(time.Second))
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// promptLine is a task's prompt as one line short enough to scan in a tree.
func promptLine(p string) string { return tools.OneLine(flow.Redact(firstLineOf(p)), 160) }

func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
