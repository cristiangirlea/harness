package watch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
						if !strings.Contains(row, want) {
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
