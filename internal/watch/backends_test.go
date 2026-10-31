package watch

import (
	"context"
	"fmt"
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
					for _, want := range []string{">default<", `<span class="quota-low">at its limit, resets in `} {
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
	for _, name := range agent.Names() {
		_, row, _ := strings.Cut(card, `data-backend="`+name+`"`)
		row, _, _ = strings.Cut(row, "</tr>")
		if !strings.Contains(row, ">unknown<") || strings.Contains(row, ` checked`) {
			t.Errorf("malformed preferences row for %s: %s", name, row)
		}
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
		{"wrong enabled type", "127.0.0.1:1234", "test-key", `{"enabled":[1]}`, "", 400},
		{"wrong default type", "127.0.0.1:1234", "test-key", `{"default":5}`, "", 400},
		{"unknown field", "127.0.0.1:1234", "test-key", `{"extra":true}`, "", 400},
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

func TestBackendsWriteDeduplicates(t *testing.T) {
	for _, existing := range []string{"", `{"enabled":["claude","claude","codex"]}`, `{broken`} {
		t.Run(existing, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			if existing != "" {
				if err := os.WriteFile(filepath.Join(runlog.Home(), "backends.json"), []byte(existing), 0600); err != nil {
					t.Fatal(err)
				}
			}
			h := New(context.Background(), Options{Key: "test-key"})
			w := backendRequest(h, "POST", "/backends", "127.0.0.1:1234", "127.0.0.1:8089", "test-key", "", `{"enabled":["codex","claude","codex","claude"]}`)
			p, err := prefs.Load(runlog.Home())
			want := []string{"codex", "claude"}
			if strings.Contains(existing, `"claude"`) {
				want = []string{"claude", "codex"}
			}
			if w.Code != 204 || err != nil || !reflect.DeepEqual(p.EnabledNames, want) {
				t.Fatalf("POST = %d, preferences = %v, %v; want %v", w.Code, p.EnabledNames, err, want)
			}
		})
	}
}

func TestBackendsIPv6LoopbackViewersCanEdit(t *testing.T) {
	for _, remote := range []string{"[::1]:1234", "[::ffff:127.0.0.1]:1234"} {
		t.Run(remote, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			h := New(context.Background(), Options{Key: "test-key"})
			card := backendCardHTML(t, backendRequest(h, "GET", "/", remote, "127.0.0.1:8089", "", "", ""))
			if !strings.Contains(card, `type="checkbox"`) {
				t.Fatal("missing edit controls")
			}
			w := backendRequest(h, "POST", "/backends", remote, "127.0.0.1:8089", "test-key", "", `{"enabled":["codex"]}`)
			if w.Code != 204 {
				t.Fatalf("POST = %d %s", w.Code, w.Body)
			}
		})
	}
}

func TestBackendsWriteRejectsCrossSiteAndUnauthenticatedPublicRequests(t *testing.T) {
	for _, tc := range []struct {
		name, token, origin, site string
		public                    bool
	}{
		{"public missing JWT", "", "", "", true},
		{"public invalid JWT", "invalid", "", "", true},
		{"public foreign origin", "valid", "https://other.example", "", true},
		{"local cross-site", "", "", "cross-site", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			opt := Options{Key: "test-key"}
			host, token := "127.0.0.1:8089", tc.token
			if tc.public {
				f := newAccessFixture(t)
				opt.Public, host = f.opt.Public, "harness.example.com"
				if token == "valid" {
					token = f.validToken()
				}
			}
			r := httptest.NewRequest("POST", "http://"+host+"/backends", strings.NewReader(`{"enabled":["codex"]}`))
			r.RemoteAddr = "127.0.0.1:1234"
			r.Header.Set("X-Hrn-Watch-Key", opt.Key)
			r.Header.Set("Cf-Access-Jwt-Assertion", token)
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Sec-Fetch-Site", tc.site)
			w := httptest.NewRecorder()
			New(context.Background(), opt).ServeHTTP(w, r)
			if w.Code != 403 {
				t.Fatalf("POST = %d %s", w.Code, w.Body)
			}
			if tc.public {
				log, err := os.ReadFile(filepath.Join(runlog.Home(), "watch-access.log"))
				if err != nil || !strings.Contains(string(log), `"method":"POST"`) || !strings.Contains(string(log), `"status":403`) || !strings.Contains(string(log), `"email":""`) {
					t.Fatalf("access log = %s, %v", log, err)
				}
			}
		})
	}
}

func TestBackendsWriteRefusesAnEmptyListWithADefaultEvenWhenStored(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: []string{"claude"}, DefaultName: "claude"}); err != nil {
		t.Fatal(err)
	}
	h := New(context.Background(), Options{Key: "test-key"})
	w := backendRequest(h, "POST", "/backends", "127.0.0.1:1234", "127.0.0.1:8089", "test-key", "", `{"enabled":[],"default":"claude"}`)
	p, _ := prefs.Load(runlog.Home())
	if w.Code != 400 || !reflect.DeepEqual(p.EnabledNames, []string{"claude"}) {
		t.Fatalf("POST = %d %s, file %v; want 400 and the file unchanged", w.Code, w.Body, p.EnabledNames)
	}
}

func TestBackendsControlsAreNotForViewersBehindAProxyOrOnTheLAN(t *testing.T) {
	for _, tc := range []struct{ name, remote, host, header string }{
		{"forwarded through a local proxy", "127.0.0.1:1234", "127.0.0.1:8089", "X-Forwarded-For"},
		{"forwarded with the standard header", "127.0.0.1:1234", "127.0.0.1:8089", "Forwarded"},
		{"cloudflare without public mode", "127.0.0.1:1234", "127.0.0.1:8089", "Cf-Connecting-Ip"},
		{"a LAN remote naming a loopback host", "192.168.1.3:1234", "127.0.0.1:8089", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			h := New(context.Background(), Options{Key: "test-key", LAN: []Endpoint{{Addr: "192.168.1.2:8089"}}})
			get := httptest.NewRequest("GET", "http://"+tc.host+"/", nil)
			get.RemoteAddr = tc.remote
			if tc.header != "" {
				get.Header.Set(tc.header, "203.0.113.9")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, get)
			if w.Code == http.StatusOK {
				card := backendCardHTML(t, w)
				if strings.Contains(card, "data-key") || strings.Contains(card, "backends-apply") {
					t.Fatalf("controls or the key shown to a %s viewer", tc.name)
				}
			}
			post := httptest.NewRequest("POST", "http://"+tc.host+"/backends", strings.NewReader(`{"enabled":["codex"]}`))
			post.RemoteAddr = tc.remote
			post.Header.Set("X-Hrn-Watch-Key", "test-key")
			if tc.header != "" {
				post.Header.Set(tc.header, "203.0.113.9")
			}
			w = httptest.NewRecorder()
			h.ServeHTTP(w, post)
			if _, err := os.Stat(filepath.Join(runlog.Home(), "backends.json")); w.Code != 403 || err == nil {
				t.Fatalf("POST = %d %s, file exists = %v; want 403 and no file", w.Code, w.Body, err == nil)
			}
		})
	}
}

func TestBackendsCardShowsEachPlanBackendsQuotaMarkingLowAndStaleWindows(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	now := time.Now()
	day := filepath.Join(codexHome, "sessions", now.Format("2006"), now.Format("01"), now.Format("02"))
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	codex := fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":95,"window_minutes":10080,"resets_at":%d},"secondary":{"used_percent":20,"window_minutes":300,"resets_at":%d}}}}`+"\n",
		now.UTC().Format(time.RFC3339Nano), now.Add(50*time.Hour).Unix(), now.Add(-5*time.Minute).Unix())
	if err := os.WriteFile(filepath.Join(day, "rollout-card.jsonl"), []byte(codex), 0o644); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(runlog.Root(), "20261015-120000-abcdef")
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "task.json"), []byte(`{"id":"20261015-120000-abcdef","backend":"claude"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	claude := fmt.Sprintf(`{"t":%q,"kind":"quota","quota":{"five_hour":{"used_percent":38,"resets_at":%d},"week":{"used_percent":20,"resets_at":%d}}}`+"\n",
		now.Format(time.RFC3339Nano), now.Add(72*time.Minute+30*time.Second).Unix(), now.Add(76*time.Hour+30*time.Minute).Unix())
	if err := os.WriteFile(filepath.Join(run, "events.jsonl"), []byte(claude), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := limits.Note("agy", agent.Result{Status: agent.StatusLimited, LimitResets: now.Add(3*time.Hour + 30*time.Second)}); err != nil {
		t.Fatal(err)
	}

	card := backendCardHTML(t, backendRequest(New(context.Background(), Options{}), "GET", "/", "127.0.0.1:1234", "127.0.0.1:8089", "", "", ""))
	row := func(name string) string {
		_, r, ok := strings.Cut(card, `data-backend="`+name+`"`)
		if !ok {
			t.Fatalf("no %s row: %s", name, card)
		}
		r, _, _ = strings.Cut(r, "</tr>")
		return html.UnescapeString(r)
	}
	if r := row("claude"); !strings.Contains(r, "5h: 62% left, resets in 1h 12m") || !strings.Contains(r, "week: 80% left, resets in 3d 4h") || strings.Contains(r, "quota-low") || strings.Contains(r, "quota-stale") {
		t.Fatalf("claude row = %s", r)
	}
	r := row("codex")
	if !strings.Contains(r, `class="quota-low">week: 5% left, resets in 2d 1h`) && !strings.Contains(r, `class="quota-low">week: 5% left, resets in 2d 2h`) {
		t.Fatalf("codex row has no low weekly window: %s", r)
	}
	if !strings.Contains(r, `class="quota-stale">5h: 80% left before its reset 5m ago (stale)`) {
		t.Fatalf("codex row has no stale five-hour window: %s", r)
	}
	// agy reports a reset only when a run hits its limit, never a share of a window.
	if r := row("agy"); !strings.Contains(r, `class="quota-low">at its limit, resets in 3h 0m`) || strings.Contains(r, "left") {
		t.Fatalf("agy row = %s", r)
	}
	if r := row("gemini"); strings.Contains(r, "left") {
		t.Fatalf("gemini has no plan quota to show: %s", r)
	}
}

func TestBackendsSavePreservesWorkMapRepos(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	if err := prefs.Save(home, prefs.Prefs{WorkMapRepos: []string{"original"}}); err != nil {
		t.Fatal(err)
	}
	h := New(context.Background(), Options{Key: "test-key"})
	for _, body := range []string{`{"enabled":["codex"]}`, `{"enabled":["claude"],"work_map_repos":["injected"]}`} {
		w := backendRequest(h, "POST", "/backends", "127.0.0.1:1234", "127.0.0.1:8089", "test-key", "", body)
		p, err := prefs.Load(home)
		if w.Code != 204 || err != nil || !reflect.DeepEqual(p.WorkMapRepos, []string{"original"}) {
			t.Fatalf("save: %d %+v %v", w.Code, p, err)
		}
	}
}
