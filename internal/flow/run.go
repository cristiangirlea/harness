package flow

import (
	"fmt"
	"strings"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/mask"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// RunStep is the step id of a run under parent, so a batch attempt and its run share one tree.
func RunStep(parent, runID string) string {
	if parent == "" {
		return "run:" + runID
	}
	return parent + "/run"
}

// FromRun rebuilds a run's steps from its record and events, which every run since the first has.
// Thinking and text are left out: they are the model's words, shown on the run page, not steps.
func FromRun(rec runlog.Record, events []agent.Event, parent string) []Event {
	root := RunStep(parent, rec.ID)
	base := Event{Owner: rec.Owner, Run: rec.ID, Derived: true}
	at := func(e Event) Event {
		e.Owner, e.Run, e.Derived = base.Owner, base.Run, true
		return Clean(e)
	}
	out := []Event{at(Event{Time: rec.Started, Step: root, Parent: parent, Kind: KindRun, Status: StatusStarted,
		Name: rec.Backend, Text: firstLine(rec.Task.Prompt), Reason: routeReason(rec)})}
	if rb := rec.RoutedBy; rb != nil {
		out = append(out, at(Event{Time: rec.Started, Step: root + "/route", Parent: root, Kind: KindRoute, Status: StatusOK,
			Name: rec.Backend, Reason: routeReason(rec)}))
	}
	var calls []openCall
	n := 0
	for _, e := range events {
		switch e.Kind {
		case agent.EventToolCall:
			n++
			step := fmt.Sprintf("%s/tool%d", root, n)
			calls = append(calls, openCall{step, e.Tool})
			out = append(out, at(Event{Time: e.Time, Step: step, Parent: root, Kind: KindTool, Status: StatusStarted,
				Name: e.Tool, Text: oneLine(e.Text, 200)}))
		case agent.EventToolResult:
			i := matchCall(calls, e.Tool)
			if i < 0 {
				out = append(out, at(Event{Time: e.Time, Step: fmt.Sprintf("%s/result%d", root, len(out)), Parent: root,
					Kind: KindTool, Status: resultStatus(e), Name: e.Tool, Text: oneLine(e.Text, 200)}))
				continue
			}
			c := calls[i]
			calls = append(calls[:i], calls[i+1:]...)
			out = append(out, at(Event{Time: e.Time, Step: c.step, Parent: root, Kind: KindTool, Status: resultStatus(e),
				Text: oneLine(e.Text, 200)}))
		case agent.EventError:
			out = append(out, at(Event{Time: e.Time, Step: fmt.Sprintf("%s/error%d", root, len(out)), Parent: root,
				Kind: KindError, Status: StatusFailed, Text: oneLine(e.Text, 400)}))
		case agent.EventInfo, agent.EventCompaction:
			name := "info"
			if e.Kind == agent.EventCompaction {
				name = "compaction"
			}
			out = append(out, at(Event{Time: e.Time, Step: fmt.Sprintf("%s/info%d", root, len(out)), Parent: root,
				Kind: KindInfo, Status: StatusOK, Name: name, Text: oneLine(e.Text, 400)}))
		}
	}
	if !rec.Done {
		return out
	}
	end := rec.Started.Add(rec.Result.Duration)
	for _, c := range calls {
		// A call with no recorded result ended with the run; say so instead of leaving it running.
		out = append(out, at(Event{Time: end, Step: c.step, Kind: KindTool, Status: StatusSkipped, Text: "no result was recorded"}))
	}
	r := rec.Result
	text := firstLine(r.Final)
	if r.Error != "" {
		text = oneLine(r.Error, 400)
	}
	out = append(out, at(Event{Time: end, Step: root, Kind: KindRun, Status: runStatus(r.Status),
		Text: fmt.Sprintf("%s · %d turns · %d tool calls · %s", r.Status, r.Turns, r.ToolCalls, text)}))
	return out
}

type openCall struct{ step, tool string }

// matchCall pairs a result with the oldest open call of its tool, or the oldest at all when unnamed.
func matchCall(calls []openCall, tool string) int {
	for i, c := range calls {
		if tool == "" || c.tool == tool {
			return i
		}
	}
	return -1
}

func resultStatus(e agent.Event) Status {
	if e.IsError {
		return StatusFailed
	}
	return StatusOK
}

// runStatus maps a run's result onto a step status.
func runStatus(s agent.Status) Status {
	switch s {
	case agent.StatusOK:
		return StatusOK
	case agent.StatusCanceled:
		return StatusCanceled
	case "":
		return StatusStarted
	}
	return StatusFailed
}

// routeReason quotes the recorded route decision; with none, the backend was asked for or a default.
func routeReason(rec runlog.Record) string {
	rb := rec.RoutedBy
	if rb == nil {
		if rec.Continues != "" {
			return "continues run " + rec.Continues
		}
		return ""
	}
	how := "classified"
	if rb.Declared {
		how = "declared"
	}
	s := fmt.Sprintf("purpose %s (%s) picked the route row set by %s", rb.Category, how, rb.Set)
	if rb.Eval != "" {
		s += " from eval " + rb.Eval
	}
	return s
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return oneLine(s, 200)
}

// oneLine masks before it shortens: a cut can remove the end marker a secret pattern needs.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(mask.Text(s)), " ")
	if r := []rune(s); len(r) > n {
		s = string(r[:n]) + "…"
	}
	return s
}
