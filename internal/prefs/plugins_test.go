package prefs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPluginPreferencesMergeTheEntireBlockAndReportLocks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	userPath := filepath.Join(home, "backends.json")
	orgPath := filepath.Join(t.TempDir(), "backends.json")
	useOrgPath(t, orgPath, true)
	for _, tc := range []struct {
		name, user, org, source string
		off, sets, ignored      bool
	}{
		{"omitted", `{}`, `{}`, "", false, false, false},
		{"inherited", `{}`, `{"plugins":{"install":"off","sources":{"echo":"Org/Repo"}}}`, "Org/Repo", true, true, false},
		{"replaced", `{"plugins":{"sources":{"echo":"User/Repo"}}}`, `{"plugins":{"install":"off","sources":{"echo":"Org/Repo"}}}`, "User/Repo", false, false, false},
		{"cleared", `{"plugins":{}}`, `{"plugins":{"install":"off"}}`, "", false, false, false},
		{"locked", `{"plugins":{"install":"on"}}`, `{"plugins":{"install":"off","sources":{"echo":"Org/Repo"}},"locked":["plugins"]}`, "Org/Repo", true, true, true},
		{"locked empty", `{"plugins":{"install":"off"}}`, `{"locked":["plugins"]}`, "", false, true, true},
		{"locked identical", `{"plugins":{"install":"off","sources":{"echo":"Org/Repo"}}}`, `{"plugins":{"install":"off","sources":{"echo":"Org/Repo"}},"locked":["plugins"]}`, "Org/Repo", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for path, raw := range map[string]string{userPath: tc.user, orgPath: tc.org} {
				if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			org, err := LoadOrg()
			if err != nil {
				t.Fatal(err)
			}
			user, err := org.LoadUser(home)
			if err != nil {
				t.Fatal(err)
			}
			p, ignored, err := Merge(org, user)
			source, ok := p.PluginSource("echo")
			if err != nil || source != tc.source || ok != (tc.source != "") ||
				p.PluginInstallOff() != tc.off || slices.Contains(ignored, "plugins") != tc.ignored ||
				slices.Contains(org.Sets(user), "plugins") != tc.sets {
				t.Fatalf("effective = %+v, ignored = %v, organisation sets = %v, error = %v", p.Plugins, ignored, org.Sets(user), err)
			}
		})
	}
}

func TestPluginPreferencesValidateSourcesAndMatchAllowEntriesCaseInsensitively(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	userPath := filepath.Join(home, "backends.json")
	orgPath := filepath.Join(t.TempDir(), "backends.json")
	useOrgPath(t, orgPath, true)
	for _, raw := range []string{
		`{"install":"private-value"}`, `{"sources":{"ECHO":"owner/repo"}}`,
		`{"sources":{"server":"owner/repo"}}`, `{"sources":{"worker":"owner/repo"}}`,
		`{"sources":{"fixture-model":"owner/repo"}}`, `{"sources":{"echo":"owner/*"}}`,
		`{"sources":{"echo":"owner/.."}}`, `{"allow":["owner/."]}`,
		`{"allow":["some_owner/repo"]}`, `{"allow":["owner/repo/extra"]}`,
		`{"allow":["owner/re*"]}`, `{"allow":["*/repo"]}`, `{"private-value":true}`,
	} {
		t.Run(raw, func(t *testing.T) {
			data := []byte(`{"plugins":` + raw + `}`)
			for _, path := range []string{userPath, orgPath} {
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Load(home); err == nil {
				t.Fatal("user preferences accepted invalid plugin settings")
			}
			if _, err := LoadOrg(); err == nil || strings.Contains(err.Error(), "private-value") {
				t.Fatalf("organisation error = %v", err)
			}
		})
	}
	p := Prefs{Plugins: &PluginPrefs{Allow: []string{"Owner/Repo", "Other/*"}}}
	for _, source := range []string{"owner/repo", "OTHER/new-repo"} {
		if !p.PluginAllowed(source) {
			t.Errorf("%q should be allowed", source)
		}
	}
	if p.PluginAllowed("owner/another") || p.PluginAllowed("others/repo") ||
		!(Prefs{}).PluginAllowed("any/repo") || !(Prefs{Plugins: &PluginPrefs{}}).PluginAllowed("any/repo") {
		t.Fatal("unexpected allowlist result")
	}
}
