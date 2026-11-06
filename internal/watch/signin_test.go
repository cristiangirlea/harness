package watch

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// visitFrom is the headers a browser sends opening a page, with an Origin when one is given.
func visitFrom(origin string) map[string]string {
	h := map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}
	if origin != "" {
		h["Origin"] = origin
	}
	return h
}

func isSignInPage(body string) bool {
	return strings.Contains(body, `<a href="/login">Sign in</a>`)
}

func TestARefusedPageVisitOffersToSignIn(t *testing.T) {
	f := newAccessFixture(t)
	h := New(context.Background(), f.opt)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	expired := f.validClaims()
	expired["exp"] = f.clock.Add(-time.Minute).Unix()
	for name, token := range map[string]string{
		"no token":      "",
		"expired":       f.token(map[string]any{"alg": "RS256", "kid": "test-key"}, expired, f.key),
		"bad signature": f.token(map[string]any{"alg": "RS256", "kid": "test-key"}, f.validClaims(), other),
	} {
		for _, origin := range []string{"", "null", "https://harness.example.com"} {
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				w := f.requestWithHeaders(h, method, "/run/20261106-120000-abcdef", "harness.example.com", "", token, visitFrom(origin))
				what := name + " " + method + " from origin " + origin
				if w.Code != http.StatusForbidden || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
					t.Errorf("%s: status %d, Content-Type %q; want a 403 page", what, w.Code, w.Header().Get("Content-Type"))
				}
				if method == http.MethodGet && !isSignInPage(w.Body.String()) {
					t.Errorf("%s: got %q, want the sign-in page", what, w.Body.String())
				}
			}
		}
	}
	w := f.requestWithHeaders(h, http.MethodGet, "/", "harness.example.com", "", "", visitFrom(""))
	body := w.Body.String()
	for _, loads := range []string{"<script", "<link", "src=", "/static/"} {
		if strings.Contains(body, loads) {
			t.Errorf("the sign-in page loads %q, which would be refused too", loads)
		}
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("the sign-in page lost its headers: %v", w.Header())
	}
}

func TestOnlyAPageVisitWithABadTokenGetsTheSignInPage(t *testing.T) {
	f := newAccessFixture(t)
	h := New(context.Background(), f.opt)
	for name, w := range map[string]*httptest.ResponseRecorder{
		"stream":         f.request(h, "/stream/index", "harness.example.com", "", ""),
		"asset":          f.requestWithHeaders(h, http.MethodGet, "/static/watch.css", "harness.example.com", "", "", map[string]string{"Sec-Fetch-Dest": "style"}),
		"post":           f.requestWithHeaders(h, http.MethodPost, "/backends", "harness.example.com", "https://harness.example.com", "", visitFrom("")),
		"wrong host":     f.requestWithHeaders(h, http.MethodGet, "/", "evil.example.com", "", "", visitFrom("")),
		"foreign origin": f.requestWithHeaders(h, http.MethodGet, "/", "harness.example.com", "", "", visitFrom("https://evil.example")),
		"cross-site":     f.requestWithHeaders(h, http.MethodGet, "/", "harness.example.com", "https://evil.example", "", map[string]string{"Sec-Fetch-Site": "cross-site"}),
		"plain fetch":    f.request(h, "/", "harness.example.com", "", ""),
	} {
		if w.Code != http.StatusForbidden || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") || strings.TrimSpace(w.Body.String()) != "Cloudflare Access login required" {
			t.Errorf("%s: status %d, Content-Type %q, body %q; want the plain 403", name, w.Code, w.Header().Get("Content-Type"), w.Body.String())
		}
	}
}

func TestSigningInAtLoginLandsOnTheIndex(t *testing.T) {
	f := newAccessFixture(t)
	h := New(context.Background(), f.opt)
	// Access passes only the cookie on paths it does not guard, so sign in with that alone.
	cookie := visitFrom("")
	cookie["Cookie"] = "CF_Authorization=" + f.validToken()
	w := f.requestWithHeaders(h, http.MethodGet, "/login", "harness.example.com", "", "", cookie)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("a signed-in /login got %d to %q (Cache-Control %q), want an uncached 303 to /", w.Code, w.Header().Get("Location"), w.Header().Get("Cache-Control"))
	}
	if w := f.requestWithHeaders(h, http.MethodGet, "/", "harness.example.com", "", "", cookie); w.Code != http.StatusOK {
		t.Errorf("the index with the cookie got %d, want 200", w.Code)
	}
	w = f.requestWithHeaders(h, http.MethodGet, "/login", "harness.example.com", "", "", visitFrom(""))
	if w.Code != http.StatusForbidden || !isSignInPage(w.Body.String()) {
		t.Errorf("an unverified /login got %d %q, want the sign-in page, not a redirect", w.Code, w.Body.String())
	}
}
