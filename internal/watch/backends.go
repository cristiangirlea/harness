package watch

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/limits"
	"github.com/cristiangirlea/harness/internal/prefs"
	"github.com/cristiangirlea/harness/internal/quota"
	"github.com/cristiangirlea/harness/internal/runlog"
)

type backendCard struct {
	Rows                 []backendRow
	Default, Chosen, Key string
	Problem              string
	Editable             bool
}

func (s *server) saveBackends(w http.ResponseWriter, r *http.Request) {
	s.actionMu.Lock()
	defer s.actionMu.Unlock()
	client := s.actionClient(w, r)
	if !s.mayChangeBackends(w, r) {
		s.writeRefusal(client, "/backends", "local requests only")
		http.Error(w, "Local requests only", http.StatusForbidden)
		return
	}
	var p *prefs.Prefs
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil || p == nil {
		s.writeRefusal(client, "/backends", "invalid preferences object")
		http.Error(w, "Expected a backend preferences object", http.StatusBadRequest)
		return
	}
	if err := d.Decode(new(any)); err != io.EOF {
		s.writeRefusal(client, "/backends", "multiple preferences objects")
		http.Error(w, "Expected one backend preferences object", http.StatusBadRequest)
		return
	}
	names := append([]string{}, p.EnabledNames...)
	if p.Default() != "" {
		names = append(names, p.Default())
	}
	for _, name := range names {
		if _, err := agent.Get(name); err != nil {
			s.writeRefusal(client, "/backends", "unknown backend")
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	had, loadErr := prefs.Load(runlog.Home())
	var old any = had
	if loadErr != nil {
		data, err := os.ReadFile(filepath.Join(runlog.Home(), "backends.json"))
		if err != nil || len(data) > 16<<10 {
			s.writeRefusal(client, "/backends", "preferences could not be read")
			http.Error(w, "Backend preferences could not be read", http.StatusInternalServerError)
			return
		}
		old = string(data)
	}

	// Preserve fallback preference order despite the card's alphabetical display.
	if len(p.EnabledNames) > 0 && len(had.EnabledNames) > 0 {
		var kept []string
		for _, name := range had.EnabledNames {
			if p.Enabled(name) && !slices.Contains(kept, name) {
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
	var unique []string
	for _, name := range p.EnabledNames {
		if !slices.Contains(unique, name) {
			unique = append(unique, name)
		}
	}
	p.EnabledNames = unique
	if p.Default() != "" && !slices.Contains(p.EnabledNames, p.Default()) {
		s.writeRefusal(client, "/backends", "default backend is not enabled")
		http.Error(w, fmt.Sprintf("default backend %s is not in enabled", p.Default()), http.StatusBadRequest)
		return
	}
	row, err := newIntent(client, old, *p)
	if err != nil || appendAction(row) != nil {
		http.Error(w, "Action audit could not be written", http.StatusInternalServerError)
		return
	}
	if err := prefs.Save(runlog.Home(), *p); err != nil {
		row.Result = "error: backend preferences could not be saved"
		_ = appendAction(row)
		// Only default-validation errors are safe to show; filesystem errors may expose paths.
		if p.Default() != "" && (len(p.EnabledNames) == 0 || !p.Enabled(p.Default())) {
			http.Error(w, err.Error(), http.StatusBadRequest)
		} else {
			http.Error(w, "Backend preferences could not be saved", http.StatusInternalServerError)
		}
		return
	}
	row.Result = "saved"
	if err := appendAction(row); err != nil {
		http.Error(w, "Action outcome could not be written", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type backendRow struct {
	Name                        string
	Installed, Enabled, Default bool
	EnabledKnown                bool
	Limit                       quota.Part   // a run ended at the plan's limit; when it lifts
	Quota                       []quota.Part // what is left of the plan, as last reported
}

// In public mode guard has verified the viewer before calling the handler.
func (s *server) mayChangeBackends(w http.ResponseWriter, r *http.Request) bool {
	if s.opt.Public != nil {
		aw, ok := w.(*accessWriter)
		return ok && aw.email != ""
	}
	// Forwarding headers reveal remote viewers hidden behind a loopback proxy.
	for _, h := range []string{"Forwarded", "X-Forwarded-For", "Cf-Connecting-Ip"} {
		if r.Header.Get(h) != "" {
			return false
		}
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
		row := backendRow{Name: name, Installed: err == nil, EnabledKnown: c.Problem == "", Enabled: c.Problem == "" && p.Enabled(name), Default: p.Default() == name}
		row.Limit = quota.Limit(limits.Until(name, now), now)
		row.Quota = quota.For(name, now).Parts(now)
		c.Rows = append(c.Rows, row)
	}
	return c
}
