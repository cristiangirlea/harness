package watch

import (
	"encoding/json"
	"html"
	"sort"
	"strings"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/mask"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// secretReplacer blanks the values of secret-looking variables in env (names
// with KEY, TOKEN, SECRET or PASSWORD in them) wherever they appear on a
// page. A run records whatever its agent saw, and an agent that ran env or
// printed a config file would put a key into it; the page may be open to the
// network. Values shorter than 12 characters are left alone: they would
// blank ordinary words and are not keys anyway. Both a value and the forms
// html/template escapes it to are replaced. Nil when there is nothing to hide.
func secretReplacer(env []string) *strings.Replacer {
	seen := map[string]bool{}
	var values []string
	for _, kv := range env {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || len(value) < 12 || !secretName(name) {
			continue
		}
		quoted, _ := json.Marshal(value) // the form a tool's JSON input holds it in
		inJSON := strings.TrimSuffix(strings.TrimPrefix(string(quoted), `"`), `"`)
		for _, v := range []string{value, inJSON, html.EscapeString(value), strings.ReplaceAll(html.EscapeString(value), "+", "&#43;")} {
			if !seen[v] {
				seen[v] = true
				values = append(values, v)
			}
		}
	}
	if len(values) == 0 {
		return nil
	}
	// The longest first, so a value inside another goes with it.
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	var pairs []string
	for _, v := range values {
		pairs = append(pairs, v, "[redacted]")
	}
	return strings.NewReplacer(pairs...)
}

func secretName(name string) bool {
	n := strings.ToUpper(name)
	for _, w := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL"} {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}

// The page cuts long text (capText, a summary's first line) before it is
// rendered, and a key cut in two is no longer the value the rendered page is
// checked for. So records are cleaned where they come in, before any cut;
// server.render's pass over the finished page stays as a backstop.

func (s *server) clean(text string) string {
	if s.redact != nil {
		text = s.redact.Replace(text)
	}
	return mask.Text(text)
}

func (s *server) cleanEvent(e agent.Event) agent.Event {
	e.Text = s.clean(e.Text)
	if len(e.Input) > 0 && s.redact != nil {
		e.Input = json.RawMessage(s.redact.Replace(string(e.Input)))
	}
	if e.Usage != nil {
		u := *e.Usage
		u.Model = s.clean(u.Model)
		e.Usage = &u
	}
	return e
}

func (s *server) cleanRecord(rec runlog.Record) runlog.Record {
	rec.Task.Prompt = s.clean(rec.Task.Prompt)
	rec.Result.Final = s.clean(rec.Result.Final)
	rec.Result.Error = s.clean(rec.Result.Error)
	return rec
}

func (s *server) cleanBatch(res batch.Result) batch.Result {
	tasks := make([]batch.TaskResult, len(res.Tasks))
	for i, t := range res.Tasks {
		t.Prompt, t.Error = s.clean(t.Prompt), s.clean(t.Error)
		attempts := make([]batch.Attempt, len(t.Attempts))
		for j, a := range t.Attempts {
			a.Prompt, a.Error = s.clean(a.Prompt), s.clean(a.Error)
			a.Result.Final, a.Result.Error = s.clean(a.Result.Final), s.clean(a.Result.Error)
			gates := make([]batch.GateResult, len(a.Gates))
			for k, g := range a.Gates {
				g.Command, g.Output = s.clean(g.Command), s.clean(g.Output)
				gates[k] = g
			}
			a.Gates = gates
			attempts[j] = a
		}
		t.Attempts = attempts
		tasks[i] = t
	}
	res.Tasks = tasks
	return res
}
