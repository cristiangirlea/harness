package watch

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/followup"
	"github.com/cristiangirlea/harness/internal/limits"
	"github.com/cristiangirlea/harness/internal/prefs"
	"github.com/cristiangirlea/harness/internal/quota"
	"github.com/cristiangirlea/harness/internal/resolve"
	"github.com/cristiangirlea/harness/internal/runlog"
)

type backendCard struct {
	Rows                 []backendRow
	Default, Chosen, Key string
	Problem              string
	Editable             bool
	Digest               string
	EnabledFixed         bool
	DefaultFixed         bool
}

func backendsDigest() (string, error) {
	data, err := prefs.Raw(runlog.Home())
	if os.IsNotExist(err) {
		return "none", nil
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func (s *server) saveBackends(w http.ResponseWriter, r *http.Request) {
	client := s.actionClient(w, r)
	if !s.mayChangeBackends(w, r) {
		s.writeRefusal(client, "/backends", "local requests only")
		http.Error(w, "Local requests only", http.StatusForbidden)
		return
	}
	var request *struct {
		prefs.Prefs
		EnabledNames *[]string `json:"enabled"`
		DefaultName  *string   `json:"default"`
		Digest       string    `json:"digest"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&request); err != nil || request == nil {
		s.writeRefusal(client, "/backends", "invalid preferences object")
		http.Error(w, "Expected a backend preferences object", http.StatusBadRequest)
		return
	}
	if err := d.Decode(new(any)); err != io.EOF {
		s.writeRefusal(client, "/backends", "multiple preferences objects")
		http.Error(w, "Expected one backend preferences object", http.StatusBadRequest)
		return
	}
	p := &request.Prefs
	if request.EnabledNames != nil {
		p.EnabledNames = *request.EnabledNames
	}
	if request.DefaultName != nil {
		p.DefaultName = *request.DefaultName
	}
	s.actionMu.Lock()
	defer s.actionMu.Unlock()
	org, err := prefs.LoadOrg()
	if err != nil {
		s.writeRefusal(client, "/backends", "preferences could not be read")
		http.Error(w, "Backend preferences could not be read", http.StatusInternalServerError)
		return
	}
	sameEnabled := sameBackendChoices(p.EnabledNames, org.EnabledNames)
	if (org.Locks("enabled") && request.EnabledNames != nil && !sameEnabled) ||
		(org.Locks("default") && request.DefaultName != nil && p.Default() != org.Default()) {
		s.writeRefusal(client, "/backends", "set by the organisation")
		http.Error(w, "Set by the organisation", http.StatusConflict)
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
	had, loadErr := org.LoadUser(runlog.Home())
	if org.Locks("enabled") || len(had.EnabledNames) == 0 && len(org.EnabledNames) > 0 && sameEnabled {
		p.EnabledNames = had.EnabledNames
	}
	if org.Locks("default") || had.DefaultName == "" && p.DefaultName == org.DefaultName {
		p.DefaultName = had.DefaultName
	}
	if !org.Locks("enabled") && (len(p.EnabledNames) > 0 || len(org.EnabledNames) == 0) &&
		p.Default() != "" && !slices.Contains(p.EnabledNames, p.Default()) {
		s.writeRefusal(client, "/backends", "default backend is not enabled")
		http.Error(w, fmt.Sprintf("default backend %s is not in enabled", p.Default()), http.StatusBadRequest)
		return
	}
	p.WorkMapRepos, p.WorkMapGitHub, p.WorkMapSessions, p.LocalAPI = had.WorkMapRepos, had.WorkMapGitHub, had.WorkMapSessions, had.LocalAPI
	p.Billing, p.BillingMaxCostUSD, p.Providers = had.Billing, had.BillingMaxCostUSD, had.Providers
	p.Accounts, p.AccountReservePercent = had.Accounts, had.AccountReservePercent

	if org.Exists() {
		if _, _, err := prefs.Merge(org, *p); err != nil {
			s.writeRefusal(client, "/backends", "invalid effective preferences")
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	digest, err := backendsDigest()
	if err != nil {
		s.writeRefusal(client, "/backends", "preferences could not be read")
		http.Error(w, "Backend preferences could not be read", http.StatusInternalServerError)
		return
	}
	if request.Digest == "" || request.Digest != digest {
		s.writeRefusal(client, "/backends", "preferences changed since the page loaded")
		http.Error(w, "Preferences changed since this page loaded: reload", http.StatusPreconditionFailed)
		return
	}
	var old any = auditPreferences(had)
	if loadErr != nil {
		data, err := prefs.Raw(runlog.Home())
		if err != nil || len(data) > 16<<10 {
			s.writeRefusal(client, "/backends", "preferences could not be read")
			http.Error(w, "Backend preferences could not be read", http.StatusInternalServerError)
			return
		}
		// The save below replaces the file, and the audit no longer holds its text.
		if err := os.WriteFile(filepath.Join(runlog.Home(), "backends.unreadable.json"), data, 0o600); err != nil {
			s.writeRefusal(client, "/backends", "unreadable preferences could not be kept")
			http.Error(w, "Backend preferences could not be read", http.StatusInternalServerError)
			return
		}
		old = unreadablePreferences
	}

	// Preserve fallback preference order despite the card's alphabetical display.
	if !org.Locks("enabled") && len(p.EnabledNames) > 0 && len(had.EnabledNames) > 0 {
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
	if !org.Locks("enabled") {
		var unique []string
		for _, name := range p.EnabledNames {
			if !slices.Contains(unique, name) {
				unique = append(unique, name)
			}
		}
		p.EnabledNames = unique
	}
	row, err := newIntent(client, old, *p)
	if err != nil || auditAppender(row) != nil {
		http.Error(w, "Action audit could not be written", http.StatusInternalServerError)
		return
	}
	if err := org.SaveUser(runlog.Home(), *p); err != nil {
		row.Result = "error: backend preferences could not be saved"
		_ = auditAppender(row)
		// Only default-validation errors are safe to show; filesystem errors may expose paths.
		if p.Default() != "" && (len(p.EnabledNames) == 0 || !p.Enabled(p.Default())) {
			http.Error(w, err.Error(), http.StatusBadRequest)
		} else {
			http.Error(w, "Backend preferences could not be saved", http.StatusInternalServerError)
		}
		return
	}
	row.Result = "saved"
	if err := auditAppender(row); err != nil {
		fmt.Fprintln(os.Stderr, "saved; outcome not audited")
	}
	w.WriteHeader(http.StatusNoContent)
}

func sameBackendChoices(submitted, policy []string) bool {
	if len(policy) == 0 {
		if len(submitted) == 0 {
			return true
		}
		policy = agent.Names()
	}
	for _, name := range submitted {
		if !slices.Contains(policy, name) {
			return false
		}
	}
	for _, name := range policy {
		if !slices.Contains(submitted, name) {
			return false
		}
	}
	return true
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
	digest, digestErr := backendsDigest()
	layers, err := resolve.BackendLayers(runlog.Home())
	p := layers.Effective
	c := backendCard{Default: p.Default(), Editable: s.mayChangeBackends(w, r), Digest: digest,
		EnabledFixed: layers.Org.Locks("enabled"), DefaultFixed: layers.Org.Locks("default")}
	if err != nil || digestErr != nil {
		c.Problem = "Backend preferences could not be read."
	} else if p.Default() == "" {
		c.Chosen = resolve.DefaultBackendNameIn(runlog.Home())
	}
	if c.Editable {
		c.Key = s.opt.Key
	}
	now := time.Now()
	for _, name := range agent.Names() {
		_, err := followup.Detect(name)
		row := backendRow{Name: name, Installed: err == nil, EnabledKnown: c.Problem == "", Enabled: c.Problem == "" && p.Enabled(name), Default: p.Default() == name}
		row.Limit = quota.Limit(limits.Until(name, now), now)
		row.Quota = quota.For(name, now).Parts(now)
		c.Rows = append(c.Rows, row)
	}
	return c
}
