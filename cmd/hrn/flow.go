package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/flow"
	"github.com/cristiangirlea/harness/internal/owner"
	"github.com/cristiangirlea/harness/internal/runlog"
)

type flowDocument struct {
	Version int          `json:"version"`
	Events  []flow.Event `json:"events"`
	Tree    []*flow.Node `json:"tree"`
}

func cmdFlow(args []string) error {
	fs := flag.NewFlagSet("flow", flag.ContinueOnError)
	level := fs.String("level", "technical", "")
	jsonOut := fs.Bool("json", false, "")
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--json", strings.HasPrefix(args[i], "--level="):
			flags = append(flags, args[i])
		case args[i] == "--level":
			if i+1 == len(args) {
				return errors.New("flag needs an argument: --level")
			}
			flags = append(flags, args[i], args[i+1])
			i++
		case strings.HasPrefix(args[i], "-"):
			flags = append(flags, args[i])
		default:
			positional = append(positional, args[i])
		}
	}
	if err := fs.Parse(append(flags, positional...)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: hrn flow <run-or-batch-id> [--level simple|technical|debug] [--json]")
	}
	id := fs.Arg(0)
	if strings.ContainsAny(id, `/\\`) || id == "" {
		return flowNotFound(id)
	}
	if *level != "simple" && *level != "technical" && *level != "debug" {
		return fmt.Errorf("invalid flow level %q: use simple, technical or debug", *level)
	}
	viewer := owner.LocalViewer()
	res, batchErr := batch.LoadResultFor(viewer, id)
	var events []flow.Event
	var roots []*flow.Node
	isBatch := batchErr == nil
	if isBatch {
		events, batchErr = batch.Timeline(viewer, id)
		if batchErr != nil {
			return flowNotFound(id)
		}
		roots = flow.Build(events)
	} else {
		rec, err := runlog.LoadFor(viewer, id)
		if err != nil {
			return flowNotFound(id)
		}
		runEvents, err := runlog.Events(id)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		events = flow.FromRun(rec, runEvents, "")
		roots = flow.Build(events)
	}
	// A flow file may predate today's redaction or come from another environment: clean it again.
	for i := range events {
		events[i] = flow.Clean(events[i])
	}
	roots = flow.Build(events)
	tree := filterFlow(roots, *level)
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(flowDocument{Version: flow.Version, Events: events, Tree: tree})
	}
	if isBatch {
		fmt.Println(safeFlowText(batchSummary(res)))
	} else {
		fmt.Println(safeFlowText(runSummary(roots)))
	}
	flow.Walk(tree, func(n *flow.Node, depth int) { printFlowNode(n, depth, *level == "debug") })
	return nil
}

// safeFlowText keeps secrets and terminal controls in records out of the terminal.
func safeFlowText(s string) string { return printable(flow.Redact(s)) }

func flowNotFound(id string) error { return fmt.Errorf("hrn flow: no run or batch %s", id) }

func runSummary(roots []*flow.Node) string {
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

func batchSummary(res batch.Result) string {
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

func filterFlow(nodes []*flow.Node, level string) []*flow.Node {
	var out []*flow.Node
	for _, n := range nodes {
		children := filterFlow(n.Children, level)
		if flowKindVisible(n.Kind, level) {
			copy := *n
			copy.Children = children
			out = append(out, &copy)
		} else {
			out = append(out, children...)
		}
	}
	return out
}

func flowKindVisible(kind flow.Kind, level string) bool {
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

func printFlowNode(n *flow.Node, depth int, debug bool) {
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
	fmt.Printf("%s%s %s\n", indent, statusMark(n.Status), safeFlowText(label))
	if n.Reason != "" {
		fmt.Printf("%s  why: %s\n", indent, safeFlowText(n.Reason))
	}
	if n.Next != "" {
		fmt.Printf("%s  next: %s\n", indent, safeFlowText(n.Next))
	}
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
