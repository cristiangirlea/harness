package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cristiangirlea/harness/internal/prefs"
)

type preferenceBackend struct {
	name      string
	installed bool
}

func (b preferenceBackend) Name() string { return b.name }
func (b preferenceBackend) Detect() (string, error) {
	if !b.installed {
		return "", errors.New("not installed")
	}
	return "stub", nil
}
func (b preferenceBackend) Run(context.Context, Task, func(Event)) Result { panic("must not run") }

func TestDefaultBackendFollowsPreferencesAndAvailability(t *testing.T) {
	original := registry
	registry = map[string]Backend{}
	t.Cleanup(func() { registry = original })
	for _, tc := range []struct {
		name       string
		p          prefs.Prefs
		api, codex bool
		want       string
	}{
		{"absent", prefs.Prefs{}, false, true, "claude"},
		{"absent api", prefs.Prefs{}, true, true, "api"},
		{"default", prefs.Prefs{EnabledNames: []string{"api", "codex"}, DefaultName: "codex"}, true, true, "codex"},
		{"api enabled", prefs.Prefs{EnabledNames: []string{"codex", "api"}}, true, true, "api"},
		{"api disabled", prefs.Prefs{EnabledNames: []string{"codex", "claude"}}, true, true, "codex"},
		{"uninstalled default", prefs.Prefs{EnabledNames: []string{"codex", "claude"}, DefaultName: "codex"}, false, false, "claude"},
		{"no installed", prefs.Prefs{EnabledNames: []string{"codex"}}, false, false, "codex"},
		{"no installed in order", prefs.Prefs{EnabledNames: []string{"codex", "api"}}, false, false, "codex"},
		{"no installed default", prefs.Prefs{EnabledNames: []string{"codex", "api"}, DefaultName: "api"}, false, false, "api"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			Register(preferenceBackend{"api", tc.api})
			Register(preferenceBackend{"codex", tc.codex})
			Register(preferenceBackend{"claude", true})
			if len(tc.p.EnabledNames) > 0 {
				if err := prefs.Save(home, tc.p); err != nil {
					t.Fatal(err)
				}
			}
			if got := DefaultBackendNameIn(home); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestBackendPreferencesRejectUnknownNamesAndMalformedFiles(t *testing.T) {
	home := t.TempDir()
	for _, raw := range []string{`{`, `{"enabled":["not-a-backend"]}`} {
		if err := os.WriteFile(filepath.Join(home, "backends.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := BackendPreferences(home); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
