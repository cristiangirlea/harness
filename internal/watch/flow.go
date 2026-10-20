package watch

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/flow"
	"github.com/cristiangirlea/harness/internal/owner"
	"github.com/cristiangirlea/harness/internal/runlog"
)

type flowPageData struct {
	page
	ID, Level, Summary string
	Tree               []flowNode
}

type flowNode struct {
	Node                  *flow.Node
	StatusClass, Duration string
	Debug                 bool
	Children              []flowNode
}

func (s *server) flowEvents(v owner.Viewer, id string) ([]flow.Event, string, error) {
	if _, err := batch.LoadResultFor(v, id); err == nil {
		events, err := batch.Timeline(v, id)
		return events, "batch", err
	}
	rec, err := runlog.LoadFor(v, id)
	if err != nil {
		return nil, "", err
	}
	events, err := runlog.Events(id)
	if err != nil && !os.IsNotExist(err) {
		return nil, "", err
	}
	return flow.FromRun(rec, events, ""), "run", nil
}

func (s *server) flowJSON(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idRE.MatchString(id) {
		http.Error(w, "flow not found", 404)
		return
	}
	events, _, err := s.flowEvents(s.who(r), id)
	if err != nil {
		http.Error(w, "flow not found", 404)
		return
	}
	events = s.prepareFlowEvents(r, events, id)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(struct {
		Version int          `json:"version"`
		Tree    []*flow.Node `json:"tree"`
	}{flow.Version, nonNilNodes(flow.Build(events))})
}

func nonNilNodes(nodes []*flow.Node) []*flow.Node {
	if nodes == nil {
		return []*flow.Node{}
	}
	return nodes
}

func (s *server) flowPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idRE.MatchString(id) {
		s.missing(w, r, "Flow "+id, os.ErrNotExist)
		return
	}
	events, kind, err := s.flowEvents(s.who(r), id)
	if err != nil {
		s.missing(w, r, "Flow "+id, os.ErrNotExist)
		return
	}
	level := r.URL.Query().Get("level")
	if level != "simple" && level != "technical" && level != "debug" {
		level = "technical"
	}
	events = s.prepareFlowEvents(r, events, id)
	summary := flowSummary(events)
	if kind == "run" {
		summary += " Open the run for tool calls and their recorded output."
	}
	p := flowPageData{page: page{Title: "Execution flow " + id, Home: s.homeFor(r)}, ID: id, Level: level, Summary: summary, Tree: buildFlowTree(events, level)}
	s.page(w, 200, "flow", p)
}

func (s *server) cleanFlowEvent(e flow.Event) flow.Event {
	e = flow.Clean(e)
	e.Name = s.redact.Replace(e.Name)
	e.Text = s.redact.Replace(e.Text)
	e.Reason = s.redact.Replace(e.Reason)
	e.Next = s.redact.Replace(e.Next)
	return e
}

func (s *server) prepareFlowEvents(r *http.Request, events []flow.Event, id string) []flow.Event {
	vw := s.viewerOf(r)
	var sh shortener
	taskShorteners := map[string]shortener{}
	if res, err := batch.LoadResultFor(s.who(r), id); err == nil {
		sh = vw.shortener(res.Dir)
		for _, task := range res.Tasks {
			taskShorteners[task.ID] = vw.shortener(task.Worktree)
		}
	} else if rec, err := runlog.LoadFor(s.who(r), id); err == nil {
		sh = vw.shortener(rec.Task.Dir)
	}
	for i := range events {
		events[i] = s.cleanFlowEvent(events[i])
		eventSh := sh
		if taskSh, ok := taskShorteners[events[i].Task]; ok {
			eventSh = taskSh
		}
		events[i].Name = eventSh.shorten(events[i].Name)
		events[i].Text = eventSh.shorten(events[i].Text)
		events[i].Reason = eventSh.shorten(events[i].Reason)
		events[i].Next = eventSh.shorten(events[i].Next)
	}
	return events
}

func buildFlowTree(events []flow.Event, level string) []flowNode {
	if level == "simple" {
		kept := events[:0]
		for _, e := range events {
			if e.Kind != flow.KindTool {
				kept = append(kept, e)
			}
		}
		events = kept
	}
	var convert func([]*flow.Node) []flowNode
	convert = func(nodes []*flow.Node) []flowNode {
		result := make([]flowNode, 0, len(nodes))
		for _, n := range nodes {
			duration := ""
			if !n.Ended.IsZero() && !n.Started.IsZero() {
				duration = short(n.Ended.Sub(n.Started))
			}
			result = append(result, flowNode{Node: n, StatusClass: statusClass(string(n.Status)), Duration: duration, Debug: level == "debug", Children: convert(n.Children)})
		}
		return result
	}
	return convert(flow.Build(events))
}

func flowSummary(events []flow.Event) string {
	passed, tasks := 0, 0
	for _, e := range events {
		if e.Kind == flow.KindTask && e.Status.Done() {
			tasks++
			if e.Status == flow.StatusOK {
				passed++
			}
		}
	}
	if tasks == 0 {
		return "No completed tasks are recorded."
	}
	text := strconv.Itoa(passed) + " of " + strconv.Itoa(tasks) + " tasks passed."
	for _, e := range events {
		if e.Kind == flow.KindGate && e.Status == flow.StatusFailed {
			text += " Task " + e.Task + " failed at gate " + e.Name + " (" + strings.TrimSpace(e.Reason) + ")."
			if e.Next != "" {
				text += " Next: " + e.Next + "."
			}
			break
		}
	}
	for _, e := range events {
		if e.Kind == flow.KindAttempt && e.Status == flow.StatusFailed && e.Text != "" {
			text += " Attempt failed: " + strings.TrimSpace(e.Text) + "."
			break
		}
	}
	return text
}
