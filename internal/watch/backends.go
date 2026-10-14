package watch

import (
	"net/http"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/limits"
	"github.com/cristiangirlea/harness/internal/runlog"
)

type backendCard struct {
	Rows                 []backendRow
	Default, Chosen, Key string
	Problem              string
	Editable             bool
}

type backendRow struct {
	Name, Limit                 string
	Installed, Enabled, Default bool
}

// In public mode guard has verified the viewer before calling the handler.
func (s *server) mayChangeBackends(w http.ResponseWriter, r *http.Request) bool {
	if s.opt.Public != nil {
		aw, ok := w.(*accessWriter)
		return ok && aw.email != ""
	}
	return s.fromThisComputer(r)
}

func (s *server) backendsCard(w http.ResponseWriter, r *http.Request) backendCard {
	p, err := agent.BackendPreferences(runlog.Home())
	c := backendCard{Default: p.Default(), Editable: s.mayChangeBackends(w, r)}
	if err != nil {
		c.Problem = "Backend preferences could not be read."
	} else if p.Default() == "" {
		c.Chosen = agent.DefaultBackendNameIn(runlog.Home())
	}
	if c.Editable {
		c.Key = s.opt.Key
	}
	now := time.Now()
	for _, name := range agent.Names() {
		b, _ := agent.Get(name)
		_, err := b.Detect()
		row := backendRow{Name: name, Installed: err == nil, Enabled: p.Enabled(name), Default: p.Default() == name}
		if until := limits.Until(name, now); !until.IsZero() {
			row.Limit = "limited until " + until.Format(time.RFC3339)
		}
		c.Rows = append(c.Rows, row)
	}
	return c
}
