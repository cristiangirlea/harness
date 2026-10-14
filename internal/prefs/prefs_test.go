package prefs

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPreferencesLoadSaveAndFilter(t *testing.T) {
	home := t.TempDir()
	p, err := Load(home)
	if err != nil || !p.Enabled("anything") || p.Default() != "" {
		t.Fatalf("absent: %+v, %v", p, err)
	}
	names := []string{"api", "claude", "codex"}
	if !reflect.DeepEqual(p.Filter(names), names) {
		t.Fatal("absent preference changed names")
	}
	want := Prefs{EnabledNames: []string{"codex", "claude"}, DefaultName: "claude"}
	for _, value := range []Prefs{want, {}, want} {
		if err := Save(home, value); err != nil {
			t.Fatal(err)
		}
		p, err = Load(home)
		if err != nil || !reflect.DeepEqual(p, value) {
			t.Fatalf("round trip: %+v, %v", p, err)
		}
	}
	if p.Enabled("api") || !p.Enabled("claude") || p.Default() != "claude" {
		t.Fatalf("methods: %+v", p)
	}
	if got := p.Filter(names); !reflect.DeepEqual(got, []string{"codex", "claude"}) {
		t.Fatalf("filter: %v", got)
	}
	if got := p.Filter([]string{"claude", "missing"}); !reflect.DeepEqual(got, []string{"claude"}) {
		t.Fatalf("intersection: %v", got)
	}
}

func TestPreferencesRefuseMalformedFilesAndDefaultsOutsideTheList(t *testing.T) {
	home := t.TempDir()
	for _, raw := range []string{`{`, `null`, `{"enabled":[],"default":"claude"}`, `{"default":"claude"}`, `{"enabled":["codex"],"default":"claude"}`} {
		if err := os.WriteFile(filepath.Join(home, "backends.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(home); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	for _, p := range []Prefs{{DefaultName: "claude"}, {EnabledNames: []string{"codex"}, DefaultName: "claude"}} {
		if err := Save(home, p); err == nil {
			t.Errorf("saved %+v", p)
		}
	}
}
