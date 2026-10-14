package watch

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/limits"
	"github.com/cristiangirlea/harness/internal/prefs"
	"github.com/cristiangirlea/harness/internal/runlog"
)

type backendCard struct {
	Rows                 []backendRow
	Default, Chosen, Key string
	Problem              string
	Editable             bool
}

func (s *server) saveBackends(w http.ResponseWriter, r *http.Request) {
	if !s.mayChangeBackends(w, r) {
		http.Error(w, "Local requests only", http.StatusForbidden)
		return
	}
	var p *prefs.Prefs
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil || p == nil {
		http.Error(w, "Expected a backend preferences object", http.StatusBadRequest)
		return
	}
	if err := d.Decode(new(any)); err != io.EOF {
		http.Error(w, "Expected one backend preferences object", http.StatusBadRequest)
		return
	}
	names := append([]string{}, p.EnabledNames...)
	if p.Default() != "" {
		names = append(names, p.Default())
	}
	for _, name := range names {
		if _, err := agent.Get(name); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	// The card lists backends alphabetically, but the list's order is the
	// preference order (the carry-on partner, the default's fallback): keep
	// what was already enabled in its order and append what is new.
	// An empty list means every backend, so there is nothing to order.
	if had, err := prefs.Load(runlog.Home()); err == nil && len(p.EnabledNames) > 0 && len(had.EnabledNames) > 0 {
		var kept []string
		for _, name := range had.EnabledNames {
			if p.Enabled(name) {
				kept = append(kept, name)
			}
		}
		for _, name := range p.EnabledNames {
			if !slices.Contains(kept, name) {
				kept = append(kept, name)
			}
		}
		p.EnabledNames = kept
	}
	if err := prefs.Save(runlog.Home(), *p); err != nil {
		// Save validates the default before touching the filesystem. Other
		// errors may include private paths, so keep them off the page.
		if p.Default() != "" && (len(p.EnabledNames) == 0 || !p.Enabled(p.Default())) {
			http.Error(w, err.Error(), http.StatusBadRequest)
		} else {
			http.Error(w, "Backend preferences could not be saved", http.StatusInternalServerError)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
