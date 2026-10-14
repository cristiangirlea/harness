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

// DefaultBackendNameIn selects an installed preferred backend. Entry points
// validate the preferences separately so malformed files are reported as errors.
// Without a preference the historical default is unchanged.
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
	for _, name := range p.Filter(Names()) {
		if installed(name) {
			return name
		}
	}
	return "claude"
}
