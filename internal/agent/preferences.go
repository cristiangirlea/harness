package agent

import "github.com/cristiangirlea/harness/internal/prefs"

// BackendPreferences loads and validates names against this binary's registry.
func BackendPreferences(home string) (prefs.Prefs, error) {
	p, err := prefs.Load(home)
	if err != nil {
		return prefs.Prefs{}, err
	}
	for _, name := range p.EnabledNames {
		if _, err := Get(name); err != nil {
			return prefs.Prefs{}, err
		}
	}
	return p, nil
}

// DefaultBackendNameIn prefers installed, enabled backends.
func DefaultBackendNameIn(home string) string {
	p, err := BackendPreferences(home)
	if err != nil || len(p.EnabledNames) == 0 {
		return DefaultBackendName()
	}
	installed := func(name string) bool {
		b, err := Get(name)
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
	enabled := p.Filter(Names())
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
