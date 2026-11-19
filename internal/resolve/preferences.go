package resolve

import (
	"errors"
	"fmt"
	"strings"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/prefs"
)

type Layers struct {
	Effective prefs.Prefs
	User      prefs.Prefs
	Org       prefs.Org
	Ignored   []string
}

func (l Layers) Notice() string {
	if len(l.Ignored) == 0 {
		return ""
	}
	return "backends: " + strings.Join(l.Ignored, ", ") + " set by the organisation; your backends.json values for them are ignored"
}

func (l Layers) OrganisationLines() []string {
	var lines []string
	for _, field := range l.Org.Sets(l.User) {
		line := fmt.Sprintf("  %s: set by the organisation", field)
		if l.Org.Locks(field) {
			line += " (locked)"
		}
		lines = append(lines, line)
	}
	return lines
}

// BackendPreferences loads and validates names against this binary's registry.
func BackendPreferences(home string) (prefs.Prefs, error) {
	layers, err := BackendLayers(home)
	return layers.Effective, err
}

func BackendLayers(home string) (Layers, error) {
	org, err := prefs.LoadOrg()
	if err != nil {
		return Layers{}, err
	}
	user, err := prefs.Load(home)
	if err != nil {
		return Layers{}, err
	}
	p, ignored, err := prefs.Merge(org, user)
	layers := Layers{User: user, Org: org, Ignored: ignored}
	if err != nil {
		return layers, err
	}
	for _, name := range p.EnabledNames {
		if _, err := agent.Get(name); err != nil {
			if org.Locks("enabled") || len(user.EnabledNames) == 0 {
				return layers, errors.New("the organisation's backends.json: enabled contains an unknown backend")
			}
			return layers, err
		}
	}
	layers.Effective = p
	return layers, nil
}

// DefaultBackendNameIn prefers installed, enabled backends.
func DefaultBackendNameIn(home string) string {
	p, err := BackendPreferences(home)
	if err != nil || len(p.EnabledNames) == 0 {
		return agent.DefaultBackendName()
	}
	installed := func(name string) bool {
		b, err := agent.Get(name)
		if err != nil {
			return false
		}
		_, err = b.Detect()
		return err == nil
	}
	if p.Default() != "" && installed(p.Default()) {
		return p.Default()
	}
	if p.Enabled("api") && installed("api") {
		return "api"
	}
	enabled := p.Filter(agent.Names())
	for _, name := range enabled {
		if installed(name) {
			return name
		}
	}
	// None is installed: name an enabled one, so the error says it is missing, not disabled.
	if p.Default() != "" {
		return p.Default()
	}
	return enabled[0] // never empty: BackendPreferences refuses unregistered names
}
