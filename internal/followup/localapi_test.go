package followup

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/prefs"
	"github.com/cristiangirlea/harness/internal/runlog"
)

const localURL = "http://127.0.0.1:8081"

// localChain installs claude, codex and agy stubs and limits codex, the run being carried on.
func localChain(t *testing.T, p prefs.Prefs) agent.Result {
	t.Helper()
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t, true, true)
	orig, _ := agent.Get("agy")
	agent.Register(stub{"agy", true})
	t.Cleanup(func() { agent.Register(orig) })
	res := agent.Result{Backend: "codex", Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)}
	if err := Note("codex", res); err != nil {
		t.Fatal(err)
	}
	if err := prefs.Save(runlog.Home(), p); err != nil {
		t.Fatal(err)
	}
	return res
}

func limitAlso(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := Note(name, agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTheLocalAPIComesAfterEveryPlanBackend(t *testing.T) {
	// api first in the list still comes last: it is the weaker model.
	res := localChain(t, prefs.Prefs{EnabledNames: []string{"api", "codex", "claude", "agy"}, LocalAPI: localURL})
	if got := Partner("codex", time.Now(), nil); got != "claude" {
		t.Fatalf("partner %q, want claude while it is ready", got)
	}
	limitAlso(t, "claude")
	if got := Partner("codex", time.Now(), nil); got != "agy" {
		t.Fatalf("partner %q, want agy while it is ready", got)
	}
	limitAlso(t, "agy")
	if got := Partner("codex", time.Now(), nil); got != "api" {
		t.Fatalf("partner %q, want api once every plan backend is limited", got)
	}
	if got, why := Choose("codex", ""); got != "api" || !strings.Contains(why, "api") {
		t.Fatalf("choice %q (%s), want api", got, why)
	}
	prev := agent.Task{Prompt: "hello", Dir: t.TempDir()}
	next, why, err := After(limitedRun(t, prev), "codex", prev, res)
	if err != nil {
		t.Fatal(err)
	}
	if next == nil || next.To != "api" {
		t.Fatalf("carry-on %+v, %s", next, why)
	}
	if got := Partner("api", time.Now(), nil); got == "api" {
		t.Fatal("api partners itself")
	}
}

func TestTheLocalAPICarriesOnWithoutAnEnabledList(t *testing.T) {
	localChain(t, prefs.Prefs{LocalAPI: localURL})
	if got := Partner("codex", time.Now(), nil); got != "claude" {
		t.Fatalf("partner %q, want claude, the usual pairing", got)
	}
	limitAlso(t, "claude")
	if got := Partner("codex", time.Now(), nil); got != "api" {
		t.Fatalf("partner %q, want api once claude is limited too", got)
	}
}

func TestWithoutTheOptInAPIStaysOutOfTheChain(t *testing.T) {
	res := localChain(t, prefs.Prefs{EnabledNames: []string{"codex", "claude", "agy", "api"}})
	limitAlso(t, "claude", "agy")
	if got := Partner("codex", time.Now(), nil); got != "claude" {
		t.Fatalf("partner %q, want the limited claude for diagnostics, as before", got)
	}
	if ok, why := Ready("api", time.Now(), nil); ok || why != "api would bill per token, not to a plan" {
		t.Fatalf("api ready %v: %s", ok, why)
	}
	prev := agent.Task{Prompt: "hello", Dir: t.TempDir()}
	if next, _, err := After(limitedRun(t, prev), "codex", prev, res); err != nil || next != nil {
		t.Fatalf("carry-on %+v, %v", next, err)
	}
	b, err := Backend("api")
	if err != nil {
		t.Fatal(err)
	}
	if api, ok := b.(*agent.APIBackend); !ok || api.BaseURL != "" || api.APIKey != "" {
		t.Fatalf("api changed without the opt-in: %+v", b)
	}
}

func TestAKeyKeepsTheLocalAPIOut(t *testing.T) {
	for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		t.Run(key, func(t *testing.T) {
			res := localChain(t, prefs.Prefs{EnabledNames: []string{"codex", "claude", "api"}, LocalAPI: localURL})
			limitAlso(t, "claude")
			t.Setenv(key, "sk-test-value")
			ok, why := Ready("api", time.Now(), nil)
			if ok || !strings.Contains(why, key) || strings.Contains(why, "sk-test-value") {
				t.Fatalf("api ready %v: %s", ok, why)
			}
			prev := agent.Task{Prompt: "hello", Dir: t.TempDir()}
			next, why, err := After(limitedRun(t, prev), "codex", prev, res)
			if err != nil || next != nil {
				t.Fatalf("carry-on %+v, %v", next, err)
			}
			if !strings.Contains(why, key) || !strings.Contains(why, "stays limited") {
				t.Fatalf("why = %q", why)
			}
			// A continuation inherits api: with a key it is refused, not sent to the paid endpoint.
			if b, err := Backend("api"); err == nil || !strings.Contains(err.Error(), key) || strings.Contains(err.Error(), "sk-test-value") {
				t.Fatalf("api with a key: %+v, %v", b, err)
			}
			if _, err := Detect("api"); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("detect with a key: %v", err)
			}
			// A plan partner kept out still says why the local api did not take over.
			if _, why, _ := Pick("codex", time.Now(), nil); !strings.Contains(why, key) {
				t.Fatalf("pick: %s", why)
			}
		})
	}
}

func TestAPublicLocalAPIIsRefused(t *testing.T) {
	localChain(t, prefs.Prefs{EnabledNames: []string{"codex", "claude", "api"}})
	limitAlso(t, "claude")
	if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: []string{"codex", "claude", "api"}, LocalAPI: "https://api.anthropic.com"}); err == nil {
		t.Fatal("a public endpoint was saved as local")
	}
	raw := `{"enabled":["codex","claude","api"],"local_api":"https://api.anthropic.com"}`
	if err := os.WriteFile(filepath.Join(runlog.Home(), "backends.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Partner("codex", time.Now(), nil); got != "" {
		t.Fatalf("partner %q from a refused preference", got)
	}
	if ok, _ := Ready("api", time.Now(), nil); ok {
		t.Fatal("api ready on a public endpoint")
	}
	if b, err := Backend("api"); err == nil {
		t.Fatalf("api ran with a refused preference: %+v", b)
	}
}

func TestADisabledAPIIgnoresTheOptIn(t *testing.T) {
	localChain(t, prefs.Prefs{EnabledNames: []string{"codex", "claude"}, LocalAPI: localURL})
	limitAlso(t, "claude")
	if ok, why := Ready("api", time.Now(), nil); ok || !strings.Contains(why, "disabled") {
		t.Fatalf("api ready %v: %s", ok, why)
	}
	if got := Partner("codex", time.Now(), nil); got != "claude" {
		t.Fatalf("partner %q, want the limited claude", got)
	}
}

func TestTheLocalAPIBackendReachesOnlyItsEndpoint(t *testing.T) {
	localChain(t, prefs.Prefs{EnabledNames: []string{"codex", "api"}, LocalAPI: localURL})
	b, err := Backend("api")
	if err != nil {
		t.Fatal(err)
	}
	api, ok := b.(*agent.APIBackend)
	if !ok || api.BaseURL != localURL || api.APIKey == "" || api.HTTPClient == nil {
		t.Fatalf("local backend %+v", b)
	}
	if orig, _ := agent.Get("api"); orig.(*agent.APIBackend).BaseURL != "" {
		t.Fatal("the registered api backend was changed")
	}
	if tr, ok := api.HTTPClient.Transport.(*http.Transport); !ok || tr.Proxy != nil {
		t.Fatal("the local endpoint may be reached through a proxy")
	}
	if err := api.HTTPClient.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirects followed: %v", err)
	}
	if src, err := Detect("api"); err != nil || !strings.Contains(src, localURL) {
		t.Fatalf("detect: %q %v", src, err)
	}
	if other, _ := Backend("codex"); other.Name() != "codex" {
		t.Fatalf("codex resolved to %s", other.Name())
	}
}

func TestUnreadablePreferencesLeaveReasonsAndDetectionAsBefore(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t, true, true)
	_ = Note("claude", agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)})
	if err := os.WriteFile(filepath.Join(runlog.Home(), "backends.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, why := Ready("claude", time.Now(), nil); ok || strings.Contains(why, "local api") {
		t.Fatalf("claude ready %v: %s", ok, why)
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-value")
	if src, err := Detect("api"); err != nil || src != "ANTHROPIC_API_KEY" {
		t.Fatalf("detect: %q %v", src, err)
	}
	if src, err := Detect("codex"); err != nil || src != "stub" {
		t.Fatalf("detect codex: %q %v", src, err)
	}
}

func TestALocalAPIRunDoesNotFollowARedirect(t *testing.T) {
	var decoy atomic.Int32
	paid := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { decoy.Add(1) }))
	defer paid.Close()
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, paid.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer local.Close()
	localChain(t, prefs.Prefs{EnabledNames: []string{"codex", "api"}, LocalAPI: local.URL})
	b, err := Backend("api")
	if err != nil {
		t.Fatal(err)
	}
	res := agent.Run(context.Background(), b, agent.Task{Prompt: "hi", Dir: t.TempDir(), Options: agent.Options{NoSwitch: true}}, func(agent.Event) {})
	if res.Status == agent.StatusOK || decoy.Load() != 0 {
		t.Fatalf("status %s, redirected requests %d", res.Status, decoy.Load())
	}
}
