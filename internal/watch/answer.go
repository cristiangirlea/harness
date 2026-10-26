package watch

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/flow"
	"github.com/cristiangirlea/harness/internal/flowview"
	"github.com/cristiangirlea/harness/internal/runlog"
)

const answerHelp = "I can only answer from what hrn recorded: why something failed, what it is doing, what happens next, how long it took."

var answerRules = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"failed", regexp.MustCompile(`^why did (.+) fail$`)},
	{"doing", regexp.MustCompile(`^what is (.+) doing$`)},
	{"doing", regexp.MustCompile(`^where is (.+)$`)},
	{"next", regexp.MustCompile(`^what(?: happens next|'s next)$`)},
	{"duration", regexp.MustCompile(`^how long did (.+) take$`)},
	{"today", regexp.MustCompile(`^what went wrong today$`)},
}

// answerScan bounds the records one question reads, each of which costs an events file.
const answerScan = 50

type botAnswer struct {
	Text string `json:"text"`
	Ref  string `json:"ref"`
	URL  string `json:"url"`
}

type answerRecord struct {
	id      string
	names   string
	started time.Time
}

func (s *server) answerBot(w http.ResponseWriter, r *http.Request) {
	a := s.recordedAnswer(r)
	a.Text = sentence(answerExcerpt(s.clean(a.Text), 199))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(a)
}

func (s *server) recordedAnswer(r *http.Request) botAnswer {
	q := strings.Join(strings.Fields(strings.ToLower(strings.Trim(strings.TrimSpace(r.URL.Query().Get("q")), "?.!"))), " ")
	q = strings.ReplaceAll(q, "’", "'")
	kind, target := "", ""
	for _, rule := range answerRules {
		if m := rule.re.FindStringSubmatch(q); m != nil {
			kind = rule.kind
			if len(m) > 1 {
				target = m[1]
			}
			break
		}
	}
	if kind == "" {
		return botAnswer{Text: answerHelp}
	}
	if target == "it" || target == "the last one" {
		target = ""
	}
	v := s.who(r)
	runs, _ := runlog.ListFor(v, 0)
	batches, _, _ := batch.ListFor(v)
	var records []answerRecord
	for _, b := range batches {
		names := b.ID
		for _, task := range b.Tasks {
			names += " " + task.ID + " " + taskName(task)
		}
		records = append(records, answerRecord{b.ID, strings.ToLower(names), b.Started})
	}
	for _, rec := range runs {
		records = append(records, answerRecord{rec.ID, strings.ToLower(rec.ID + " " + rec.Task.Prompt), rec.Started})
	}
	slices.SortStableFunc(records, func(a, b answerRecord) int {
		if c := b.started.Compare(a.started); c != 0 {
			return c
		}
		return strings.Compare(a.id, b.id)
	})
	var first botAnswer
	failedToday, scanned := 0, 0
	now := time.Now()
	for _, rec := range records {
		if target != "" && !strings.Contains(rec.names, target) {
			continue
		}
		// A count must be exact over the window it names, so today's window is the bound, not the cap.
		if kind == "today" {
			if rec.started.Before(now.Add(-48 * time.Hour)) {
				break
			}
		} else if scanned++; scanned > answerScan {
			break
		}
		view, err := flowview.Load(v, rec.id)
		if err != nil || len(view.Tree) == 0 {
			continue
		}
		// The same shortening as the flow page: a full path names the account that ran it.
		view.Tree = flow.Build(s.prepareFlowEvents(r, view.Events, rec.id))
		if len(view.Tree) == 0 {
			continue
		}
		root := view.Tree[0]
		cause := answerFailure(view.Tree)
		var text string
		switch kind {
		case "failed":
			if cause == "" {
				continue
			}
			text = rec.id + " " + cause
		case "doing":
			if root.Status.Done() || !root.Ended.IsZero() {
				continue
			}
			step := answerCurrent(view.Tree)
			if step == nil {
				continue
			}
			text = rec.id + " is at " + answerStep(step)
		case "next":
			var next, waiting string
			flow.Walk(view.Tree, func(n *flow.Node, _ int) {
				if n.Next != "" {
					next = n.Next
				}
				if n.Status == flow.StatusWaiting && n.Ended.IsZero() {
					waiting = answerStep(n)
					if n.Reason != "" {
						waiting += ": " + n.Reason
					}
				}
			})
			switch {
			case next != "":
				text = rec.id + " next: " + next
			case waiting != "":
				text = rec.id + " is waiting at " + waiting
			default:
				text = rec.id + " has no next step recorded"
			}
		case "duration":
			if root.Started.IsZero() || root.Ended.IsZero() {
				text = rec.id + " has no completed duration recorded"
			} else {
				text = rec.id + " took " + spoken(root.Ended.Sub(root.Started))
			}
		case "today":
			at := root.Ended.In(now.Location())
			if cause == "" || root.Ended.IsZero() || at.Format("2006-01-02") != now.Format("2006-01-02") {
				continue
			}
			failedToday++
			if first.Ref == "" {
				first = botAnswer{Text: cause, Ref: rec.id, URL: "/flow/" + rec.id}
			}
			continue
		}
		return botAnswer{Text: text, Ref: rec.id, URL: "/flow/" + rec.id}
	}
	if first.Ref != "" {
		first.Text = fmt.Sprintf("%s: %d failed records today among those started in the last two days; %s", first.Ref, failedToday, first.Text)
		return first
	}
	return botAnswer{Text: answerHelp}
}

func answerFailure(tree []*flow.Node) string {
	if tree[0].Status != flow.StatusFailed {
		return ""
	}
	var cause string
	flow.Walk(tree, func(n *flow.Node, _ int) {
		if cause == "" && n.Kind == flow.KindTask && n.Status == flow.StatusFailed {
			cause = "task " + n.Name + " failed" + failureOf(n)
		}
	})
	if cause == "" {
		cause = answerStep(tree[0]) + " failed"
	}
	return cause
}

func answerCurrent(tree []*flow.Node) *flow.Node {
	var last *flow.Node
	flow.Walk(tree, func(n *flow.Node, _ int) {
		if !n.Status.Done() && n.Ended.IsZero() && !n.Started.IsZero() && (last == nil || !n.Started.Before(last.Started)) {
			last = n
		}
	})
	return last
}

func answerStep(n *flow.Node) string {
	text := string(n.Kind)
	if n.Name != "" {
		text += " " + n.Name
	}
	if n.Text != "" {
		text += ": " + n.Text
	}
	return text
}

func answerExcerpt(text string, limit int) string {
	runes := []rune(strings.Join(strings.Fields(text), " "))
	if len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return string(runes)
}
