package flowview

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/flow"
	"github.com/cristiangirlea/harness/internal/owner"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/ws"
)

// ErrNotFound covers missing, unreadable and foreign ids alike, so callers cannot tell them apart.
var ErrNotFound = errors.New("no run or batch")

// View is a cleaned execution tree and its source summary.
type View struct {
	Version int
	Summary string
	Issues  []ws.IssueStatus `json:"issues,omitempty"`
	Events  []flow.Event
	Tree    []*flow.Node
}

// IssuesOf lists the redacted issue leases of run or batch id and its tasks, from disk alone so a refreshing view never calls GitHub.
// The caller has checked that v may load id; the runs id continues are checked here.
func IssuesOf(v owner.Viewer, id string) []ws.IssueStatus {
	all, err := ws.ReadIssues(runlog.Home())
	if err != nil {
		return nil
	}
	ids := continuedRuns(v, id)
	var out []ws.IssueStatus
	for _, s := range all {
		if slices.Contains(ids, s.Run) || strings.HasPrefix(s.Run, id+"/") {
			out = append(out, redactIssue(s))
		}
	}
	return out
}

const maxContinues = 20

// A backend switch continues the run under a new id while the lease keeps the first one.
// The walk stops at the first run v may not load, so nothing of another owner's chain shows.
func continuedRuns(v owner.Viewer, id string) []string {
	ids := []string{id}
	rec, err := runlog.LoadFor(v, id)
	for range maxContinues {
		if err != nil || rec.Continues == "" || slices.Contains(ids, rec.Continues) {
			break
		}
		prev := rec.Continues
		if rec, err = runlog.LoadFor(v, prev); err != nil {
			break
		}
		ids = append(ids, prev)
	}
	return ids
}

func redactIssue(s ws.IssueStatus) ws.IssueStatus {
	for _, f := range []*string{&s.Issue, &s.Why, &s.Holder, &s.Run, &s.Backend, &s.Branch, &s.PR} {
		*f = flow.Redact(*f)
	}
	return s
}

// IssueLine says who holds an issue, for the text and page views.
func IssueLine(s ws.IssueStatus) string {
	if s.State == "held by pull request" {
		return "issue " + s.Issue + ": held by pull request " + s.PR
	}
	line := "issue " + s.Issue + ": held by hrn " + s.Run
	if s.Backend != "" {
		line += " (" + s.Backend + ")"
	}
	if s.Branch != "" {
		line += " on " + s.Branch
	}
	if !s.Since.IsZero() {
		line += " since " + s.Since.Local().Format("2006-01-02 15:04")
	}
	if s.State == "stale" {
		line += ", stale: " + s.Why
	}
	return line
}

// Load resolves an id with the same batch-first rules as hrn flow.
func Load(v owner.Viewer, id string) (View, error) {
	if id == "" || strings.ContainsAny(id, `/\\`) {
		return View{}, fmt.Errorf("%w %s", ErrNotFound, id)
	}
	res, err := batch.LoadResultFor(v, id)
	isBatch := err == nil
	var events []flow.Event
	var summary string
	if isBatch {
		events, err = batch.Timeline(v, id)
		if err != nil {
			return View{}, fmt.Errorf("%w %s", ErrNotFound, id)
		}
		summary = BatchSummary(res)
	} else {
		rec, loadErr := runlog.LoadFor(v, id)
		if loadErr != nil {
			return View{}, fmt.Errorf("%w %s", ErrNotFound, id)
		}
		runEvents, eventErr := runlog.Events(id)
		if eventErr != nil && !errors.Is(eventErr, os.ErrNotExist) {
			return View{}, eventErr
		}
		events = flow.FromRun(rec, runEvents, "")
	}
	for i := range events {
		events[i] = flow.Clean(events[i])
	}
	roots := flow.Build(events)
	if !isBatch {
		summary = RunSummary(roots)
	}
	return View{Version: flow.Version, Summary: flow.Redact(summary), Issues: IssuesOf(v, id), Events: events, Tree: roots}, nil
}

// Filter returns a tree containing only the steps visible at level.
func Filter(nodes []*flow.Node, level string) []*flow.Node {
	var out []*flow.Node
	for _, n := range nodes {
		children := Filter(n.Children, level)
		if kindVisible(n.Kind, level) {
			copy := *n
			copy.Children = children
			out = append(out, &copy)
		} else {
			out = append(out, children...)
		}
	}
	return out
}

func kindVisible(kind flow.Kind, level string) bool {
	switch kind {
	case flow.KindBatch, flow.KindTask, flow.KindAttempt, flow.KindRun, flow.KindGate, flow.KindDecision, flow.KindRoute, flow.KindCommit, flow.KindError:
		return true
	case flow.KindTool:
		return level != "simple"
	case flow.KindInfo:
		return level == "debug"
	default:
		return false
	}
}

// RunSummary describes a run's root step.
func RunSummary(roots []*flow.Node) string {
	if len(roots) == 0 {
		return "run: status unknown"
	}
	n := roots[0]
	status := string(n.Status)
	if status == "" {
		status = "started"
	}
	line := "run " + n.Run + ": " + status
	if n.Text != "" {
		line += " — " + n.Text
	}
	return line
}

// BatchSummary includes the recorded reason for failed tasks.
func BatchSummary(res batch.Result) string {
	passed, failed, canceled := 0, 0, 0
	for _, t := range res.Tasks {
		switch t.Status {
		case batch.StatusPass:
			passed++
		case batch.StatusFail, batch.StatusError:
			failed++
		case batch.StatusCanceled:
			canceled++
		}
	}
	line := fmt.Sprintf("batch %s: %d passed, %d failed, %d canceled", res.ID, passed, failed, canceled)
	for _, t := range res.Tasks {
		if t.Status != batch.StatusFail && t.Status != batch.StatusError {
			continue
		}
		var reason string
		for i := len(t.Attempts) - 1; i >= 0 && reason == ""; i-- {
			for j := len(t.Attempts[i].Gates) - 1; j >= 0; j-- {
				g := t.Attempts[i].Gates[j]
				if g.ExitCode != 0 {
					reason = fmt.Sprintf("gate %s: %s", g.Command, g.Outcome())
					break
				}
			}
			if reason == "" && t.Attempts[i].Result.Error != "" {
				reason = "run: " + t.Attempts[i].Result.Error
			}
			if reason == "" && t.Attempts[i].Error != "" {
				reason = t.Attempts[i].Error
			}
		}
		if reason == "" {
			reason = t.Error
		}
		if reason != "" {
			line += fmt.Sprintf("\n%s: %s", t.ID, reason)
		}
	}
	return line
}

// WriteText renders the summary, issue holders and tree in hrn flow's text format.
func WriteText(w io.Writer, summary string, issues []ws.IssueStatus, tree []*flow.Node, level string) error {
	if _, err := fmt.Fprintln(w, summary); err != nil {
		return err
	}
	for _, s := range issues {
		if _, err := fmt.Fprintln(w, IssueLine(s)); err != nil {
			return err
		}
	}
	var writeErr error
	flow.Walk(tree, func(n *flow.Node, depth int) {
		if writeErr != nil {
			return
		}
		writeErr = writeNode(w, n, depth, level == "debug")
	})
	return writeErr
}

func writeNode(w io.Writer, n *flow.Node, depth int, debug bool) error {
	indent := strings.Repeat("  ", depth)
	label := string(n.Kind)
	if n.Name != "" {
		label += " " + n.Name
	}
	if debug {
		label += fmt.Sprintf(" [step=%s", n.Step)
		if n.Run != "" {
			label += " run=" + n.Run
		}
		if n.Derived {
			label += " derived"
		}
		label += "]"
	}
	if n.Orphan {
		label += " (gap: parent not recorded)"
	}
	if !n.Ended.IsZero() {
		label += " " + n.Ended.Sub(n.Started).Round(time.Millisecond).String()
	}
	if n.Text != "" {
		label += " — " + n.Text
	}
	if _, err := fmt.Fprintf(w, "%s%s %s\n", indent, statusMark(n.Status), label); err != nil {
		return err
	}
	if n.Reason != "" {
		if _, err := fmt.Fprintf(w, "%s  why: %s\n", indent, n.Reason); err != nil {
			return err
		}
	}
	if n.Next != "" {
		if _, err := fmt.Fprintf(w, "%s  next: %s\n", indent, n.Next); err != nil {
			return err
		}
	}
	return nil
}

func statusMark(status flow.Status) string {
	switch status {
	case flow.StatusOK:
		return "✓"
	case flow.StatusFailed:
		return "✗"
	case flow.StatusCanceled:
		return "⊘"
	case flow.StatusStarted, flow.StatusWaiting:
		return "…"
	case flow.StatusSkipped:
		return "-"
	default:
		return "…"
	}
}
