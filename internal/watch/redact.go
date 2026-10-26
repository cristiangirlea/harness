package watch

import (
	"encoding/json"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/mask"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// Redact before truncation splits secrets; the finished-page pass is only a backstop.
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
		// The page backstop masks environment values, not everything mask.Text finds.
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
