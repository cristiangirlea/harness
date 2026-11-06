package prefs

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestALocalAPIEndpointMustBeLoopbackOrPrivate(t *testing.T) {
	home := t.TempDir()
	for _, url := range []string{"http://127.0.0.1:8081", "http://localhost:8081/", "http://[::1]:8081", "https://192.168.1.20:8443", "http://10.0.0.5", "http://172.16.3.4:8081", "http://[fd00::5]:8081"} {
		if err := Save(home, Prefs{EnabledNames: []string{"claude", "api"}, LocalAPI: url}); err != nil {
			t.Errorf("%s refused: %v", url, err)
		}
		if p, err := Load(home); err != nil || p.LocalAPI != url {
			t.Errorf("%s did not round-trip: %+v %v", url, p, err)
		}
	}
	for _, url := range []string{"https://api.anthropic.com", "http://8.8.8.8:8081", "http://inference.lan:8081", "http://0.0.0.0:8081", "http://169.254.1.1",
		"ftp://127.0.0.1", "127.0.0.1:8081", "http://user:secret@127.0.0.1:8081", "http://127.0.0.1:8081?x=1", "http://127.0.0.1:8081?", "http://127.0.0.1:8081#x", "http://127.0.0.1:8081#", "http://127.0.0.1.example.com"} {
		err := Save(home, Prefs{LocalAPI: url})
		if err == nil {
			t.Errorf("%s accepted as local", url)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("the error shows the URL's password: %v", err)
		}
		if err := os.WriteFile(filepath.Join(home, "backends.json"), []byte(`{"local_api":"`+url+`"}`), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(home); err == nil {
			t.Errorf("a hand-written %s loaded", url)
		}
	}
}
