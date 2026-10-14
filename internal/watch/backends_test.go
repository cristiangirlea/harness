package watch

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/limits"
	"github.com/cristiangirlea/harness/internal/prefs"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func backendRequest(h http.Handler, method, path, remote, host, key, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://"+host+path, strings.NewReader(body))
	r.RemoteAddr = remote
	r.Header.Set("X-Hrn-Watch-Key", key)
	r.Header.Set("Cf-Access-Jwt-Assertion", token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func backendCardHTML(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("index = %d: %s", w.Code, w.Body)
	}
	_, card, ok := strings.Cut(w.Body.String(), `<section id="backends"`)
	if !ok {
		t.Fatal("index has no Backends card")
	}
	card, _, _ = strings.Cut(card, "</section>")
	return card
}

func TestBackendsCardShowsStateAndControlsOnlyToAllowedViewers(t *testing.T) {
	for _, viewer := range []string{"loopback", "lan", "public"} {
		t.Run(viewer, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			opt := Options{Key: "test-watch-key", LAN: []Endpoint{{Addr: "192.168.1.2:8089"}}}
			remote, host, token := "127.0.0.1:1234", "127.0.0.1:8089", ""
			if viewer == "lan" {
				remote, host = "192.168.1.3:1234", "192.168.1.2:8089"
			}
			if viewer == "public" {
				f := newAccessFixture(t)
				opt.Public, host, token = f.opt.Public, "harness.example.com", f.validToken()
			}
			if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: []string{"claude"}, DefaultName: "claude"}); err != nil {
				t.Fatal(err)
			}
			until := time.Now().Add(time.Hour).Truncate(time.Second)
			if err := limits.Note("claude", agent.Result{Status: agent.StatusLimited, LimitResets: until}); err != nil {
				t.Fatal(err)
			}
			h := New(context.Background(), opt)
			card := backendCardHTML(t, backendRequest(h, "GET", "/", remote, host, "", token, ""))
			for _, name := range agent.Names() {
				_, row, ok := strings.Cut(card, `data-backend="`+name+`"`)
				row, _, _ = strings.Cut(row, "</tr>")
				if !ok {
					t.Errorf("missing backend %s", name)
				}
				b, _ := agent.Get(name)
				installed := "installed"
				if _, err := b.Detect(); err != nil {
					installed = "not installed"
				}
				enabled := "disabled"
				if name == "claude" {
					enabled = "enabled"
					for _, want := range []string{">default<", "limited until " + until.Format(time.RFC3339)} {
						if !strings.Contains(html.UnescapeString(row), want) {
							t.Errorf("claude row lacks %q", want)
						}
					}
				}
				for _, want := range []string{">" + installed + "<", ">" + enabled + "<"} {
					if !strings.Contains(row, want) {
						t.Errorf("%s row lacks %q", name, want)
					}
				}
			}
			for _, control := range []string{`type="checkbox"`, `type="radio"`, ">Apply</button>", `data-key="` + opt.Key + `"`} {
				if strings.Contains(card, control) != (viewer != "lan") {
					t.Errorf("%s: wrong visibility for %s", viewer, control)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			stream := backendRequest(New(ctx, opt), "GET", "/stream/index", remote, host, "", token, "")
			if strings.Contains(stream.Body.String(), opt.Key) || strings.Contains(stream.Body.String(), `data-key=`) {
				t.Fatal("stream exposed the write key")
			}
		})
	}
}

func TestBackendsCardShowsChosenDefaultAndMalformedNotice(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	h := New(context.Background(), Options{})
	get := func() string {
		return backendCardHTML(t, backendRequest(h, "GET", "/", "127.0.0.1:1234", "127.0.0.1:8089", "", "", ""))
	}
	if card := get(); !strings.Contains(card, "chosen: "+agent.DefaultBackendNameIn(runlog.Home())) {
		t.Fatal("missing chosen default")
	}
	if err := os.WriteFile(filepath.Join(runlog.Home(), "backends.json"), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	card := get()
	if strings.Count(card, "Backend preferences could not be read.") != 1 || strings.Contains(card, runlog.Home()) || strings.Contains(card, "backends.json") {
		t.Fatalf("malformed notice must appear once and name no path: %s", card)
	}
}

func TestBackendsWritesRequireAnAllowedViewerAndValidPreferences(t *testing.T) {
	for _, tc := range []struct {
		name, remote, key, body, message string
		status                           int
	}{
		{"no key", "127.0.0.1:1234", "", `{"enabled":["codex"],"default":"codex"}`, "", 403},
		{"wrong key", "127.0.0.1:1234", "wrong", `{"enabled":["codex"]}`, "", 403},
		{"LAN with key", "192.168.1.3:1234", "test-key", `{"enabled":["codex"]}`, "Local requests only", 403},
		{"unknown enabled", "127.0.0.1:1234", "test-key", `{"enabled":["unknown"]}`, "unknown backend", 400},
		{"empty backend name", "127.0.0.1:1234", "test-key", `{"enabled":[""]}`, "unknown backend", 400},
		{"unknown default", "127.0.0.1:1234", "test-key", `{"enabled":["codex"],"default":"unknown"}`, "unknown backend", 400},
		{"disabled default", "127.0.0.1:1234", "test-key", `{"enabled":["codex"],"default":"claude"}`, "not in enabled", 400},
		{"empty enabled with default", "127.0.0.1:1234", "test-key", `{"enabled":[],"default":"codex"}`, "not in enabled", 400},
		{"malformed", "127.0.0.1:1234", "test-key", `{broken`, "", 400},
		{"null", "127.0.0.1:1234", "test-key", `null`, "", 400},
		{"trailing JSON", "127.0.0.1:1234", "test-key", `{} {}`, "", 400},
		{"oversized", "127.0.0.1:1234", "test-key", `{"enabled":["` + strings.Repeat("x", 17000) + `"]}`, "", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: []string{"claude"}}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(runlog.Home(), "backends.json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			h := New(context.Background(), Options{Key: "test-key", LAN: []Endpoint{{Addr: "192.168.1.2:8089"}}})
			host := "127.0.0.1:8089"
			if strings.HasPrefix(tc.remote, "192.") {
				host = "192.168.1.2:8089"
			}
			w := backendRequest(h, "POST", "/backends", tc.remote, host, tc.key, "", tc.body)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.message) {
				t.Errorf("POST = %d %s; want %d %s", w.Code, w.Body, tc.status, tc.message)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(before) {
				t.Fatalf("refused write changed preferences: %v", err)
			}
		})
	}
}

func TestBackendsWriteUpdatesPreferencesAndRepairsMalformedFiles(t *testing.T) {
	for _, viewer := range []string{"loopback", "public", "malformed"} {
		t.Run(viewer, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			opt := Options{Key: "test-key"}
			host, token := "127.0.0.1:8089", ""
			if viewer == "public" {
				f := newAccessFixture(t)
				opt.Public, host, token = f.opt.Public, "harness.example.com", f.validToken()
			}
			if viewer == "malformed" {
				if err := os.WriteFile(filepath.Join(runlog.Home(), "backends.json"), []byte("{broken"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			h := New(context.Background(), opt)
			w := backendRequest(h, "POST", "/backends", "127.0.0.1:1234", host, opt.Key, token, `{"enabled":["codex"],"default":"codex"}`)
			if w.Code != 204 || w.Body.Len() != 0 {
				t.Fatalf("POST = %d %s", w.Code, w.Body)
			}
			p, err := agent.BackendPreferences(runlog.Home())
			if err != nil || !reflect.DeepEqual(p.EnabledNames, []string{"codex"}) || p.Default() != "codex" {
				t.Fatalf("new runs see %+v, %v", p, err)
			}
			card := backendCardHTML(t, backendRequest(h, "GET", "/", "127.0.0.1:1234", host, "", token, ""))
			_, row, _ := strings.Cut(card, `data-backend="codex"`)
			row, _, _ = strings.Cut(row, "</tr>")
			if !strings.Contains(row, ">enabled<") || !strings.Contains(row, ">default<") || strings.Contains(card, "could not be read") {
				t.Fatal("index did not show saved preferences")
			}
			if viewer == "public" {
				log, err := os.ReadFile(filepath.Join(runlog.Home(), "watch-access.log"))
				if err != nil || !strings.Contains(string(log), `"method":"POST"`) || !strings.Contains(string(log), `"email":"owner@example.com"`) || !strings.Contains(string(log), `"status":204`) {
					t.Fatalf("public write was not logged: %s, %v", log, err)
				}
			}
			w = backendRequest(h, "POST", "/backends", "127.0.0.1:1234", host, opt.Key, token, `{"enabled":[],"default":""}`)
			p, err = agent.BackendPreferences(runlog.Home())
			if w.Code != 204 || err != nil || p.Default() != "" || len(p.EnabledNames) != 0 || !p.Enabled("claude") {
				t.Fatalf("reset = %d, %+v, %v", w.Code, p, err)
			}
		})
	}
}

func TestBackendsWriteKeepsThePreferenceOrderAndAppendsNewOnes(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: []string{"claude", "agy"}, DefaultName: "claude"}); err != nil {
		t.Fatal(err)
	}
	h := New(context.Background(), Options{Key: "test-key"})
	// The card lists backends alphabetically; applying it must not reorder what was already enabled.
	w := backendRequest(h, "POST", "/backends", "127.0.0.1:1234", "127.0.0.1:8089", "test-key", "", `{"enabled":["agy","claude","codex"],"default":"claude"}`)
	if w.Code != 204 {
		t.Fatalf("POST = %d %s", w.Code, w.Body)
	}
	p, err := prefs.Load(runlog.Home())
	if err != nil || !reflect.DeepEqual(p.EnabledNames, []string{"claude", "agy", "codex"}) {
		t.Fatalf("order after the card = %v, %v; want claude, agy, codex", p.EnabledNames, err)
	}
}
