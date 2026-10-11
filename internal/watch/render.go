package watch

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/qr"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/tools"
)

// Everything from a record reaches the page through html/template, which
// escapes it for where it lands; tool results hold arbitrary file contents.
//
//go:embed assets
var assets embed.FS

var pages = template.Must(template.ParseFS(assets, "assets/*.tmpl"))

// render runs one template to a string.
func render(name string, data any) (string, error) {
	var b bytes.Buffer
	err := pages.ExecuteTemplate(&b, name, data)
	return b.String(), err
}

// How much of a long text a page shows: the start and the end, where a
// command's errors usually are. The whole text stays in events.jsonl.
const (
	textHead, textTail     = 24 << 10, 8 << 10
	resultHead, resultTail = 12 << 10, 4 << 10
	inputHead, inputTail   = 6 << 10, 2 << 10
)

type fact struct{ Label, Value string }

type page struct {
	Title string
	Home  string
}

type eventView struct {
	Class   string
	Label   string
	When    string // RFC 3339, for the datetime attribute
	Clock   string
	Gap     string // time since the step before
	Slow    bool
	Text    bool // the assistant's own words: shown, not collapsed
	Summary string
	Body    string
	More    string // what the cap left out
}

// eventOf renders one line of a run's timeline. prev is the time of the step
// before (the run's start for the first); it returns the time to pass on.
func eventOf(l Line, prev time.Time) (eventView, time.Time) {
	if l.Err != nil {
		return eventView{Class: "ev-bad", Label: "unreadable", Summary: l.Err.Error()}, prev
	}
	e := l.Event
	var v eventView
	if !e.Time.IsZero() {
		v.When, v.Clock = e.Time.Format(time.RFC3339Nano), e.Time.Local().Format("15:04:05")
		if d := e.Time.Sub(prev); !prev.IsZero() && d >= 0 {
			v.Gap, v.Slow = Gap(d), d >= 30*time.Second
		}
		prev = e.Time
	}
	switch e.Kind {
	case agent.EventText:
		v.Class, v.Label, v.Text = "ev-text", "text", true
		v.Body, v.More = capText(e.Text, textHead, textTail)
	case agent.EventThinking:
		v.Class, v.Label, v.Summary = "ev-thinking", "thinking", tools.OneLine(e.Text, 140)
		v.Body, v.More = capText(e.Text, textHead, textTail)
	case agent.EventToolCall:
		v.Class, v.Label, v.Summary = "ev-call", e.Tool, callSummary(e)
		if v.Label == "" {
			v.Label = "tool"
		}
		v.Body, v.More = capText(prettyJSON(e.Input), inputHead, inputTail)
	case agent.EventToolResult:
		v.Class, v.Label, v.Summary = "ev-result", "result", resultSummary(e.Text)
		if e.IsError {
			v.Class, v.Label = "ev-result ev-failed", "failed"
		}
		v.Body, v.More = capText(e.Text, resultHead, resultTail)
	default: // info, error, and any kind added later
		v.Class, v.Label, v.Summary = "ev-info", string(e.Kind), tools.OneLine(e.Text, 300)
		if e.Kind == agent.EventError {
			v.Class = "ev-error"
		}
		if v.Label == "" {
			v.Label = "info"
		}
		if v.Summary != strings.TrimSpace(e.Text) {
			v.Body, v.More = capText(e.Text, textHead, textTail)
		}
	}
	return v, prev
}

// callSummary is the one line a tool call shows: the backend's own summary,
// the input field that says what it does, or the input itself.
func callSummary(e agent.Event) string {
	s := e.Text
	if s == "" {
		s = tools.InputSummary(e.Input)
	}
	if s == "" && len(e.Input) > 0 {
		var v any
		if json.Unmarshal(e.Input, &v) == nil {
			b, _ := json.Marshal(v)
			s = string(b)
		} else {
			s = string(e.Input)
		}
	}
	return tools.OneLine(s, 200)
}

func resultSummary(s string) string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return "(empty)"
	}
	first, _, _ := strings.Cut(trimmed, "\n")
	out := tools.OneLine(first, 120)
	if n := strings.Count(trimmed, "\n") + 1; n > 1 {
		out += fmt.Sprintf(" · %d lines", n)
	}
	if len(s) >= 1<<10 {
		out += " · " + size(len(s))
	}
	return out
}

func prettyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var b bytes.Buffer
	if json.Indent(&b, raw, "", "  ") != nil {
		return string(raw)
	}
	return b.String()
}

// capText keeps the first head and last tail bytes of s, on rune boundaries,
// and says how much it left out.
func capText(s string, head, tail int) (string, string) {
	if len(s) <= head+tail {
		return s, ""
	}
	h := head
	for h > 0 && !utf8.RuneStart(s[h]) {
		h--
	}
	t := len(s) - tail
	for t < len(s) && !utf8.RuneStart(s[t]) {
		t++
	}
	return s[:h] + "\n…\n" + s[t:], fmt.Sprintf("%s of %s not shown here; the whole text is in the run's events.jsonl", size(t-h), size(len(s)))
}

func size(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// Gap is the wait before a step, as the page and hrn show --follow print it.
func Gap(d time.Duration) string { return "+" + short(d) }

// short is a duration as a person reads it: 0.4s, 12.3s, 2m05s, 1h02m.
func short(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		d = d.Round(time.Second)
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	d = d.Round(time.Minute)
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// cost is a cost as the terminal summary prints it, ~ marking hrn's own
// estimate; none reported (codex, gemini, or nothing finished yet) is "-".
func cost(usd float64, estimate bool) string {
	if usd <= 0 {
		return "-"
	}
	if estimate {
		return fmt.Sprintf("~$%.4f", usd)
	}
	return fmt.Sprintf("$%.4f", usd)
}

// tokens is a count as the terminal summary prints it: 340, 1.2k, 3.4M.
func tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

// statusClass picks the badge colour for a run, task or gate status.
func statusClass(s string) string {
	switch s {
	case string(agent.StatusOK), string(batch.StatusPass):
		return "ok"
	case string(batch.StatusRunning), "":
		return "run"
	case string(agent.StatusError), string(agent.StatusRefused), string(batch.StatusFail):
		return "bad"
	}
	return "warn"
}

// firstLine is the first line of a prompt that has something on it.
func firstLine(s string) (line string, more bool) {
	trimmed := strings.TrimSpace(s)
	first, rest, _ := strings.Cut(trimmed, "\n")
	line = tools.OneLine(first, 140)
	return line, rest != "" || line != strings.TrimSpace(first)
}

type resultView struct {
	Status, Class string
	Facts         []fact
	Error         string
	Final         string
	FinalMore     string
}

func resultOf(res agent.Result) resultView {
	v := resultView{Status: string(res.Status), Class: statusClass(string(res.Status)), Error: res.Error}
	add := func(label, value string) { v.Facts = append(v.Facts, fact{label, value}) }
	backend := res.Backend
	if res.Model != "" {
		backend += " · " + res.Model
	}
	add("Backend", backend)
	add("Turns", fmt.Sprint(res.Turns))
	add("Tool calls", fmt.Sprint(res.ToolCalls))
	add("Tokens", fmt.Sprintf("%s in · %s out · %s cache read · %s cache write",
		tokens(res.InputTokens), tokens(res.OutputTokens), tokens(res.CacheRead), tokens(res.CacheWrite)))
	add("Cost", cost(res.CostUSD, res.CostEstimate))
	add("Duration", short(res.Duration))
	if res.Session != "" {
		add("Session", res.Session)
	}
	if !res.LimitResets.IsZero() {
		add("Limit lifts", res.LimitResets.Local().Format("2006-01-02 15:04"))
	}
	v.Final, v.FinalMore = capText(res.Final, textHead, textTail)
	return v
}

type runPage struct {
	page
	ID, Continues string
	PromptLine    string
	Prompt        string
	PromptMore    bool
	Facts         []fact
	Since         string // the start, for the live clock while the run goes
	Last          string // the last event's time, for "last step … ago"
	Events        []eventView
	Result        *resultView
	Stream        string // where the rest comes from; empty once it ended
}

func runPageOf(rec runlog.Record, events []eventView, last time.Time) runPage {
	p := runPage{page: page{Title: "Run " + rec.ID, Home: runlog.Home()}, ID: rec.ID, Continues: rec.Continues, Prompt: rec.Task.Prompt, Events: events}
	p.PromptLine, p.PromptMore = firstLine(rec.Task.Prompt)
	model := rec.Task.Model
	if rec.Done && rec.Result.Model != "" {
		model = rec.Result.Model
	}
	backend := rec.Backend
	if model != "" {
		backend += " · " + model
	}
	p.Facts = []fact{{"Backend", backend}, {"Mode", string(rec.Task.Mode)}, {"Workspace", rec.Task.Dir},
		{"Started", rec.Started.Local().Format("2006-01-02 15:04:05")}}
	if rec.Done {
		r := resultOf(rec.Result)
		p.Result = &r
	} else {
		p.Since = rec.Started.Format(time.RFC3339Nano)
		if !last.IsZero() {
			p.Last = last.Format(time.RFC3339Nano)
		}
	}
	return p
}

type indexPage struct {
	page
	Lists lists
	Local bool        // seen from this computer, not a phone on the network
	Phone []phoneLink // where a phone opens it, with hrn watch --lan
}

// phoneLink is one network address of the page, with a QR code of it to scan.
type phoneLink struct {
	Endpoint
	QRView int    // the code's side in modules, its quiet zone included
	QRPath string // the dark modules, an SVG path
}

func phoneLinks(eps []Endpoint) []phoneLink {
	const quiet = 4
	var out []phoneLink
	for _, ep := range eps {
		l := phoneLink{Endpoint: ep}
		if c, err := qr.Encode(ep.URL); err == nil {
			l.QRView, l.QRPath = c.Size+2*quiet, c.SVGPath(quiet)
		}
		out = append(out, l)
	}
	return out
}

type lists struct {
	Runs     []row
	Batches  []row
	Problems []string
}

// row is one run or batch in a list.
type row struct {
	ID, Kind, Status, Class string
	Backend                 string
	Tasks                   string
	Took                    string // the duration, or the start while it goes
	Cost                    string // what the finished runs cost, as far as their backends say
	Since                   string // the start, for the live clock while it goes
	Prompt                  string
	Batch                   string // for a run: the batch whose attempt it was
}

func runRow(rec runlog.Record) row {
	r := row{ID: rec.ID, Kind: "run", Backend: rec.Backend}
	r.Prompt, _ = firstLine(rec.Task.Prompt)
	if rec.Done {
		r.Status, r.Took, r.Cost = string(rec.Result.Status), short(rec.Result.Duration), cost(rec.Result.CostUSD, rec.Result.CostEstimate)
	} else {
		r.Status, r.Took, r.Since = "running", "since "+rec.Started.Local().Format("15:04"), rec.Started.Format(time.RFC3339Nano)
	}
	r.Class = statusClass(r.Status)
	return r
}

func batchRow(res batch.Result) row {
	r := row{ID: res.ID, Kind: "batch"}
	pass, going := 0, 0
	var backends []string
	var usd float64
	estimate := false
	for _, t := range res.Tasks {
		switch t.Status {
		case batch.StatusPass:
			pass++
		case batch.StatusRunning:
			going++
		}
		for _, a := range t.Attempts {
			if !contains(backends, a.Backend) {
				backends = append(backends, a.Backend)
			}
			usd += a.Result.CostUSD
			estimate = estimate || a.Result.CostEstimate
		}
	}
	r.Backend = strings.Join(backends, ", ")
	r.Cost = cost(usd, estimate)
	r.Tasks = fmt.Sprintf("%d/%d passed", pass, len(res.Tasks))
	if going > 0 {
		r.Tasks += fmt.Sprintf(" · %d running", going)
	}
	if batchGoing(res) {
		r.Status, r.Took, r.Since = "running", "since "+res.Started.Local().Format("15:04"), res.Started.Format(time.RFC3339Nano)
	} else {
		r.Status, r.Took = "done", short(res.Duration)
		if pass < len(res.Tasks) {
			r.Status = "failed"
		}
	}
	r.Class = statusClass(r.Status)
	if r.Status == "failed" {
		r.Class = "bad"
	}
	if len(res.Tasks) > 0 {
		r.Prompt, _ = firstLine(res.Tasks[0].Prompt)
		if len(res.Tasks) > 1 {
			r.Prompt += fmt.Sprintf(" (and %d more)", len(res.Tasks)-1)
		}
	}
	return r
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// batchGoing: a task is still running, or the batch has not written its end.
func batchGoing(res batch.Result) bool {
	for _, t := range res.Tasks {
		if t.Status == batch.StatusRunning {
			return true
		}
	}
	return res.Duration == 0
}

type batchPage struct {
	page
	ID     string
	Stream string
	Body   batchBody
}

type batchBody struct {
	Status, Class string
	Facts         []fact
	Since         string
	Tasks         []taskView
}

type taskView struct {
	ID, Status, Class  string
	PromptLine, Prompt string
	PromptMore         bool
	Facts              []fact
	Error              string
	Waiting            bool // running, with no attempt yet: queued for a worker
	Attempts           []attemptView
}

type attemptView struct {
	N                     int
	Backend, RunID        string
	Status, Class, Detail string
	Error                 string
	Continued, Running    bool
	SwitchedFrom          string
	Gates                 []gateView
}

type gateView struct {
	Key, Command, Exit, Class, Took string
	Output, More                    string
}

func batchBodyOf(res batch.Result) batchBody {
	r := batchRow(res)
	b := batchBody{Status: r.Status, Class: r.Class, Since: r.Since}
	b.Facts = []fact{{"Repository", res.Dir}, {"Base", tools.ShortSHA(res.Base)}, {"Tasks", r.Tasks},
		{"Parallel", fmt.Sprint(res.Parallel)}, {"Started", res.Started.Local().Format("2006-01-02 15:04:05")}}
	if !batchGoing(res) {
		b.Facts = append(b.Facts, fact{"Duration", short(res.Duration)})
	}
	for _, t := range res.Tasks {
		tv := taskView{ID: t.ID, Status: string(t.Status), Class: statusClass(string(t.Status)), Prompt: t.Prompt, Error: t.Error,
			Waiting: t.Status == batch.StatusRunning && len(t.Attempts) == 0}
		tv.PromptLine, tv.PromptMore = firstLine(t.Prompt)
		tv.Facts = []fact{{"Branch", t.Branch}}
		if t.Commit != "" {
			tv.Facts = append(tv.Facts, fact{"Commit", tools.ShortSHA(t.Commit)})
		}
		if t.Worktree != "" {
			tv.Facts = append(tv.Facts, fact{"Worktree", t.Worktree})
		}
		if t.Status != batch.StatusRunning {
			tv.Facts = append(tv.Facts, fact{"Took", short(t.Duration)})
		}
		for i, a := range t.Attempts {
			av := attemptView{N: i + 1, Backend: a.Backend, RunID: a.RunID, Error: a.Error, Continued: a.Continued,
				SwitchedFrom: a.SwitchedFrom, Running: a.Result.Status == "" && t.Status == batch.StatusRunning}
			av.Status = string(a.Result.Status)
			switch {
			case av.Running:
				av.Status = "running"
			case av.Status == "":
				av.Status = "no result"
			}
			av.Class = statusClass(av.Status)
			if !av.Running && a.Result.Status != "" {
				d := []string{fmt.Sprintf("%d turns", a.Result.Turns), fmt.Sprintf("%d tool calls", a.Result.ToolCalls)}
				if a.Result.CostUSD > 0 {
					d = append(d, cost(a.Result.CostUSD, a.Result.CostEstimate))
				}
				d = append(d, short(a.Result.Duration))
				av.Detail = strings.Join(d, " · ")
				if a.Result.Error != "" {
					av.Error = strings.TrimSpace(a.Result.Error + "\n" + a.Error)
				}
			}
			for j, g := range a.Gates {
				gv := gateView{Key: fmt.Sprintf("%s/%d/%d", t.ID, i+1, j+1), Command: g.Command, Exit: fmt.Sprint(g.ExitCode), Took: short(g.Duration)}
				gv.Class = "ok"
				switch {
				case g.ExitCode == -1:
					gv.Exit, gv.Class = "killed", "bad"
				case g.ExitCode != 0:
					gv.Class = "bad"
				}
				gv.Output, gv.More = capText(g.Output, resultHead, resultTail)
				av.Gates = append(av.Gates, gv)
			}
			tv.Attempts = append(tv.Attempts, av)
		}
		b.Tasks = append(b.Tasks, tv)
	}
	return b
}

func batchPageOf(res batch.Result) batchPage {
	p := batchPage{page: page{Title: "Batch " + res.ID, Home: runlog.Home()}, ID: res.ID, Body: batchBodyOf(res)}
	if batchGoing(res) {
		p.Stream = "/stream/batch/" + res.ID
	}
	if res.Dir != "" {
		p.Title = fmt.Sprintf("Batch of %d in %s", len(res.Tasks), filepath.Base(res.Dir))
	}
	return p
}
