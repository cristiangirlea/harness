package prefs

import (
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
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

func TestSaveWaitsForAReaderThatHoldsTheFile(t *testing.T) {
	home := t.TempDir()
	if err := Save(home, Prefs{EnabledNames: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	// Windows refuses to replace a file another process has open.
	f, err := os.Open(filepath.Join(home, "backends.json"))
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() { time.Sleep(100 * time.Millisecond); f.Close(); close(released) }()
	want := Prefs{EnabledNames: []string{"codex"}}
	err = Save(home, want)
	<-released
	if err != nil {
		t.Fatalf("save while the file was open: %v", err)
	}
	if p, err := Load(home); err != nil || !reflect.DeepEqual(p, want) {
		t.Fatalf("after save: %+v, %v", p, err)
	}
}

func TestLoadsDuringSavesDoNotFail(t *testing.T) {
	home := t.TempDir()
	if err := Save(home, Prefs{EnabledNames: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := Save(home, Prefs{EnabledNames: []string{"claude", "codex"}}); err != nil {
				t.Errorf("save: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := Load(home); err != nil {
				t.Errorf("load: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestOldPreferencesLoadWithoutWorkMapRepos(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "backends.json"), []byte(`{"enabled":["codex"],"default":"codex"}`), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := Load(home)
	if err != nil || p.Default() != "codex" || p.WorkMapRepos != nil {
		t.Fatalf("%+v %v", p, err)
	}
}
