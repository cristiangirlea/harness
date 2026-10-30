package watch

import (
	"bufio"
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/owner"
	"github.com/cristiangirlea/harness/internal/runlog"
)

type accessFixture struct {
	t        *testing.T
	key      *rsa.PrivateKey
	extraKey atomic.Pointer[rsa.PrivateKey]
	certs    *httptest.Server
	fetches  atomic.Int32
	fail     atomic.Bool
	pauseAt  atomic.Int32
	started  chan struct{}
	release  chan struct{}
	clock    time.Time
	opt      Options
}

func newAccessFixture(t *testing.T) *accessFixture {
	t.Helper()
	t.Setenv("HRN_HOME", t.TempDir())
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &accessFixture{t: t, key: key, clock: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
	f.certs = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetch := f.fetches.Add(1)
		if fetch == f.pauseAt.Load() {
			close(f.started)
			<-f.release
		}
		if f.fail.Load() {
			http.Error(w, "unavailable", http.StatusInternalServerError)
			return
		}
		if r.URL.Path != "/cdn-cgi/access/certs" {
			http.NotFound(w, r)
			return
		}
		n := base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes())
		keys := []any{map[string]string{"kid": "test-key", "kty": "RSA", "n": n, "e": e}}
		if extra := f.extraKey.Load(); extra != nil {
			keys = append(keys, map[string]string{
				"kid": "new-key", "kty": "RSA",
				"n": base64.RawURLEncoding.EncodeToString(extra.PublicKey.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(extra.PublicKey.E)).Bytes()),
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	}))
	t.Cleanup(f.certs.Close)
	f.opt.Public = &PublicAccess{Origin: "https://harness.example.com", Team: "team.cloudflareaccess.com", Audience: "aud-tag", CertsURL: f.certs.URL + "/cdn-cgi/access/certs", Now: func() time.Time { return f.clock }}
	return f
}

func (f *accessFixture) token(header map[string]any, claims map[string]any, key *rsa.PrivateKey) string {
	f.t.Helper()
	encode := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			f.t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	message := encode(header) + "." + encode(claims)
	if key == nil {
		return message + "."
	}
	digest := sha256.Sum256([]byte(message))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		f.t.Fatal(err)
	}
	return message + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (f *accessFixture) validClaims() map[string]any {
	return map[string]any{"aud": "aud-tag", "iss": "https://team.cloudflareaccess.com", "exp": f.clock.Add(time.Hour).Unix(), "iat": f.clock.Add(-time.Minute).Unix(), "email": "owner@example.com"}
}

func (f *accessFixture) validToken() string {
	return f.token(map[string]any{"alg": "RS256", "kid": "test-key"}, f.validClaims(), f.key)
}

func (f *accessFixture) request(h http.Handler, path, host, origin, token string) *httptest.ResponseRecorder {
	return f.requestWithHeaders(h, http.MethodGet, path, host, origin, token, nil)
}

func (f *accessFixture) requestWithHeaders(h http.Handler, method, path, host, origin, token string, headers map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, "http://127.0.0.1:8089"+path, nil)
	r.Host = host
	r.RemoteAddr = "127.0.0.1:5678"
	r.Header.Set("Cf-Connecting-IP", "198.51.100.8")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if token != "" {
		r.Header.Set("Cf-Access-Jwt-Assertion", token)
	}
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestPublicOwnerEmailAccessIsolatedByVerifiedEmail(t *testing.T) {
	f := newAccessFixture(t)
	t.Setenv("HRN_OWNER", "local:test")
	runID := saveRun(t, "fake", "mine", &agent.Result{Status: agent.StatusOK})
	batchID := "20261020-120000-aabbcc"
	saveBatch(t, batch.Result{ID: batchID, Owner: owner.Local(), Started: time.Now()})
	f.opt.OwnerEmails = []string{"OWNER@example.com"}
	h := New(context.Background(), f.opt)
	ownerResp := f.request(h, "/run/"+runID, "harness.example.com", "", f.validToken())
	if ownerResp.Code != 200 {
		t.Fatalf("owner: %d", ownerResp.Code)
	}
	ownerBatch := f.request(h, "/batch/"+batchID, "harness.example.com", "", f.validToken())
	if ownerBatch.Code != 200 {
		t.Fatalf("owner batch: %d", ownerBatch.Code)
	}
	for _, path := range []string{"/run/" + runID, "/flow/" + runID, "/flow/" + batchID, "/batch/" + batchID, "/stream/run/" + runID, "/stream/batch/" + batchID, "/stream/flow/" + runID} {
		claims := f.validClaims()
		claims["email"] = "other@example.com"
		token := f.token(map[string]any{"alg": "RS256", "kid": "test-key"}, claims, f.key)
		resp := f.request(h, path, "harness.example.com", "", token)
		if resp.Code != http.StatusNotFound || strings.Contains(resp.Body.String(), runID) && !strings.Contains(path, runID) {
			t.Errorf("%s: %d %s", path, resp.Code, resp.Body)
		}
		if path == "/stream/flow/"+runID && strings.Contains(resp.Header().Get("Content-Type"), "text/event-stream") {
			t.Errorf("stranger flow stream sent event-stream headers")
		}
	}
	claims := f.validClaims()
	claims["email"] = "other@example.com"
	token := f.token(map[string]any{"alg": "RS256", "kid": "test-key"}, claims, f.key)
	resp := f.request(h, "/", "harness.example.com", "", token)
	if strings.Contains(resp.Body.String(), runID) || strings.Contains(resp.Body.String(), batchID) {
		t.Fatal("stranger index listed local records")
	}
}

func TestParsePublicAccess(t *testing.T) {
	cases := []struct {
		name, origin, team, audience string
		wantOrigin, wantTeam         string
		wantErr                      bool
	}{
		{"normalizes origin and team", "https://HOST.Example.COM:443", "TEAM.CloudflareAccess.COM", "aud-tag", "https://host.example.com", "team.cloudflareaccess.com", false},
		{"http", "http://host.example.com", "team.cloudflareaccess.com", "aud-tag", "", "", true},
		{"path", "https://host.example.com/path", "team.cloudflareaccess.com", "aud-tag", "", "", true},
		{"query", "https://host.example.com?x=1", "team.cloudflareaccess.com", "aud-tag", "", "", true},
		{"userinfo", "https://user@host.example.com", "team.cloudflareaccess.com", "aud-tag", "", "", true},
		{"wrong team", "https://host.example.com", "team.example.com", "aud-tag", "", "", true},
		{"audience whitespace", "https://host.example.com", "team.cloudflareaccess.com", "aud tag", "", "", true},
		{"empty origin", "", "team.cloudflareaccess.com", "aud-tag", "", "", true},
		{"empty team", "https://host.example.com", "", "aud-tag", "", "", true},
		{"empty audience", "https://host.example.com", "team.cloudflareaccess.com", "", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePublicAccess(tc.origin, tc.team, tc.audience)
			if tc.wantErr {
				if err == nil || got != nil {
					t.Fatalf("ParsePublicAccess() = %+v, %v; want error", got, err)
				}
				return
			}
			if err != nil || got == nil {
				t.Fatalf("ParsePublicAccess() = %+v, %v; want success", got, err)
			}
			if got.Origin != tc.wantOrigin || got.Team != tc.wantTeam || got.Audience != tc.audience {
				t.Fatalf("ParsePublicAccess() = %+v", got)
			}
		})
	}
}

func TestPublicAccessChecksEveryRequestAndKeepsItRemote(t *testing.T) {
	f := newAccessFixture(t)
	h := New(context.Background(), f.opt)
	token := f.validToken()
	w := f.request(h, "/?private=yes", "harness.example.com:443", "https://harness.example.com", token)
	if w.Code != 200 {
		t.Fatalf("valid request: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), runlog.Home()) || strings.Contains(w.Body.String(), "--lan") || strings.Contains(w.Body.String(), "scan with your phone") {
		t.Fatal("public index exposed local details")
	}
	if w.Header().Get("Strict-Transport-Security") != "max-age=31536000" {
		t.Fatal("missing HSTS")
	}
	if got := f.request(h, "/voice", "harness.example.com", "", token); got.Code != 403 {
		t.Fatalf("voice = %d", got.Code)
	}
	if got := f.request(h, "/bot", "harness.example.com", "", token); got.Code != 200 || strings.Contains(got.Body.String(), "\"Local\":true") {
		t.Fatalf("bot = %d, local flag leaked", got.Code)
	}
	if got := f.request(h, "/static/watch.css", "harness.example.com", "", ""); got.Code != 403 {
		t.Fatalf("static without token = %d", got.Code)
	}
	cookieRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8089/static/watch.css", nil)
	cookieRequest.Host = "harness.example.com"
	cookieRequest.AddCookie(&http.Cookie{Name: "CF_Authorization", Value: token})
	cookieResponse := httptest.NewRecorder()
	h.ServeHTTP(cookieResponse, cookieRequest)
	if cookieResponse.Code != 200 {
		t.Fatalf("cookie fallback = %d", cookieResponse.Code)
	}
	for _, tc := range []struct{ name, host, origin, token, reason string }{
		{"missing token", "harness.example.com", "", "", ""},
		{"wrong host", "other.example", "", token, "bad host"},
		{"loopback host", "127.0.0.1:8089", "", token, "bad host"},
		{"wrong origin scheme", "harness.example.com", "http://harness.example.com", token, "bad origin"},
		{"wrong origin host", "harness.example.com", "https://other.example", token, "bad origin"},
		{"host with a trailing dot", "harness.example.com.", "", token, "bad host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := f.request(h, "/", tc.host, tc.origin, tc.token); got.Code != 403 {
				t.Fatalf("status = %d", got.Code)
			}
			if tc.reason != "" {
				if got := lastAccessRow(t)["reason"]; got != tc.reason {
					t.Fatalf("access log reason = %v, want %s", got, tc.reason)
				}
			}
		})
	}
	if got := f.request(h, "/", "HARNESS.Example.COM", "", token); got.Code != 200 {
		t.Fatalf("upper-case host = %d %s", got.Code, got.Body)
	}
	crossSite := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8089/", nil)
	crossSite.Host = "harness.example.com"
	crossSite.Header.Set("Cf-Access-Jwt-Assertion", token)
	crossSite.Header.Set("Sec-Fetch-Site", "cross-site")
	crossSiteResponse := httptest.NewRecorder()
	h.ServeHTTP(crossSiteResponse, crossSite)
	if crossSiteResponse.Code != 403 {
		t.Fatalf("cross-site = %d", crossSiteResponse.Code)
	}
}

func TestPublicAccessAllowsPageNavigationAfterLoginAndKeepsOtherGuards(t *testing.T) {
	f := newAccessFixture(t)
	f.opt.Key = "k3y"
	h := New(context.Background(), f.opt)
	token := f.validToken()
	for _, tc := range []struct {
		name, method, host, origin, token string
		headers                           map[string]string
		want                              int
	}{
		{"cross-site GET navigation", http.MethodGet, "harness.example.com", "", token, map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document", "Sec-Fetch-Site": "cross-site"}, 200},
		{"cross-site iframe navigation", http.MethodGet, "harness.example.com", "", token, map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "iframe", "Sec-Fetch-Site": "cross-site"}, 403},
		{"cross-site navigation without destination", http.MethodGet, "harness.example.com", "", token, map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "cross-site"}, 403},
		{"null-origin GET navigation", http.MethodGet, "harness.example.com", "null", token, map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}, 200},
		{"cross-site HEAD navigation", http.MethodHead, "harness.example.com", "", token, map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document", "Sec-Fetch-Site": "cross-site"}, 200},
		{"navigation without Access token", http.MethodGet, "harness.example.com", "", "", map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document", "Sec-Fetch-Site": "cross-site"}, 403},
		{"cross-site CORS GET", http.MethodGet, "harness.example.com", "", token, map[string]string{"Sec-Fetch-Mode": "cors", "Sec-Fetch-Site": "cross-site"}, 403},
		{"cross-site POST navigation", http.MethodPost, "harness.example.com", "", token, map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "cross-site", "X-Hrn-Watch-Key": "k3y"}, 403},
		{"navigation with wrong Host", http.MethodGet, "other.example", "", token, map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "cross-site"}, 403},
		{"navigation with the public Origin", http.MethodGet, "harness.example.com", "https://harness.example.com", token, map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}, 200},
		{"navigation with another Origin", http.MethodGet, "harness.example.com", "https://other.example", token, map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}, 403},
		{"navigation with another Origin and cross-site", http.MethodGet, "harness.example.com", "https://other.example", token, map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document", "Sec-Fetch-Site": "cross-site"}, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := f.requestWithHeaders(h, tc.method, "/", tc.host, tc.origin, tc.token, tc.headers)
			if got.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", got.Code, tc.want, got.Body)
			}
			if tc.want == http.StatusForbidden && strings.Contains(tc.name, "Origin") {
				if reason := lastAccessRow(t)["reason"]; reason != "bad origin" {
					t.Errorf("access log reason = %v, want bad origin", reason)
				}
			}
			if tc.name == "cross-site iframe navigation" || tc.name == "cross-site navigation without destination" {
				data, err := os.ReadFile(filepath.Join(runlog.Home(), "watch-access.log"))
				if err != nil {
					t.Fatal(err)
				}
				lines := strings.Split(strings.TrimSpace(string(data)), "\n")
				var row struct {
					Reason string `json:"reason"`
				}
				if err := json.Unmarshal([]byte(lines[len(lines)-1]), &row); err != nil {
					t.Fatal(err)
				}
				if row.Reason != "bad origin" {
					t.Errorf("access log reason = %q, want bad origin", row.Reason)
				}
			}
			if tc.want == http.StatusOK {
				if frameOptions := got.Header().Get("X-Frame-Options"); frameOptions != "DENY" {
					t.Errorf("X-Frame-Options = %q, want DENY", frameOptions)
				}
				if policy := got.Header().Get("Content-Security-Policy"); !strings.Contains(policy, "frame-ancestors 'none'") {
					t.Errorf("Content-Security-Policy = %q, want frame-ancestors 'none'", policy)
				}
			}
		})
	}
}

func TestPublicAccessRefusesANonLoopbackListenerEvenIfItIsListedAsLAN(t *testing.T) {
	f := newAccessFixture(t)
	lo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lo.Close()
	addr := &net.TCPAddr{IP: net.ParseIP("192.168.1.20"), Port: 8089}
	ln := addrListener{lo, addr}
	f.opt.LAN = []Endpoint{{Addr: addr.String()}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := Serve(ctx, f.opt, ln); err == nil {
		t.Fatal("public mode accepted a LAN listener")
	}
}

func TestPublicAccessRejectsInvalidJWTClaimsAndSignatures(t *testing.T) {
	f := newAccessFixture(t)
	h := New(context.Background(), f.opt)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name           string
		header, claims map[string]any
		key            *rsa.PrivateKey
	}{
		{"bad signature", map[string]any{"alg": "RS256", "kid": "test-key"}, f.validClaims(), other},
		{"alg none", map[string]any{"alg": "none", "kid": "test-key"}, f.validClaims(), nil},
		{"alg HS256", map[string]any{"alg": "HS256", "kid": "test-key"}, f.validClaims(), f.key},
		{"expired", map[string]any{"alg": "RS256", "kid": "test-key"}, func() map[string]any { c := f.validClaims(); c["exp"] = f.clock.Add(-time.Minute).Unix(); return c }(), f.key},
		{"future nbf", map[string]any{"alg": "RS256", "kid": "test-key"}, func() map[string]any { c := f.validClaims(); c["nbf"] = f.clock.Add(2 * time.Minute).Unix(); return c }(), f.key},
		{"future iat", map[string]any{"alg": "RS256", "kid": "test-key"}, func() map[string]any { c := f.validClaims(); c["iat"] = f.clock.Add(2 * time.Minute).Unix(); return c }(), f.key},
		{"wrong aud", map[string]any{"alg": "RS256", "kid": "test-key"}, func() map[string]any { c := f.validClaims(); c["aud"] = "elsewhere"; return c }(), f.key},
		{"wrong iss", map[string]any{"alg": "RS256", "kid": "test-key"}, func() map[string]any { c := f.validClaims(); c["iss"] = "https://wrong.example"; return c }(), f.key},
		{"absent aud", map[string]any{"alg": "RS256", "kid": "test-key"}, func() map[string]any { c := f.validClaims(); delete(c, "aud"); return c }(), f.key},
		{"null aud", map[string]any{"alg": "RS256", "kid": "test-key"}, func() map[string]any { c := f.validClaims(); c["aud"] = nil; return c }(), f.key},
		{"number aud", map[string]any{"alg": "RS256", "kid": "test-key"}, func() map[string]any { c := f.validClaims(); c["aud"] = 42; return c }(), f.key},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := f.request(h, "/", "harness.example.com", "", f.token(tc.header, tc.claims, tc.key))
			if w.Code != 403 || strings.TrimSpace(w.Body.String()) != "Cloudflare Access login required" {
				t.Fatalf("status %d body %q", w.Code, w.Body.String())
			}
			if strings.HasSuffix(tc.name, " aud") {
				if reason := lastAccessRow(t)["reason"]; reason != "wrong aud" {
					t.Fatalf("access log reason = %v, want wrong aud", reason)
				}
			}
		})
	}
	c := f.validClaims()
	c["aud"] = []string{"another", "aud-tag"}
	if w := f.request(h, "/", "harness.example.com", "", f.token(map[string]any{"alg": "RS256", "kid": "test-key"}, c, f.key)); w.Code != 200 {
		t.Fatalf("array audience = %d", w.Code)
	}
}

func TestPublicAccessRefetchesAnUnknownKeyAtMostOncePerMinute(t *testing.T) {
	f := newAccessFixture(t)
	h := New(context.Background(), f.opt)
	f.request(h, "/", "harness.example.com", "", f.validToken())
	unknown := f.token(map[string]any{"alg": "RS256", "kid": "unknown"}, f.validClaims(), f.key)
	for range 2 {
		if w := f.request(h, "/", "harness.example.com", "", unknown); w.Code != 403 {
			t.Fatalf("unknown kid = %d", w.Code)
		}
	}
	if got := f.fetches.Load(); got != 2 {
		t.Fatalf("fetches = %d, want initial and one refetch", got)
	}
	newKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f.extraKey.Store(newKey)
	f.clock = f.clock.Add(61 * time.Second)
	newToken := f.token(map[string]any{"alg": "RS256", "kid": "new-key"}, f.validClaims(), newKey)
	if w := f.request(h, "/", "harness.example.com", "", newToken); w.Code != 200 {
		t.Fatalf("new kid after one minute = %d: %s", w.Code, w.Body)
	}
	if got := f.fetches.Load(); got != 3 {
		t.Fatalf("fetches = %d, want initial, first unknown and after one minute", got)
	}
}

func TestPublicAccessRefusesToServeWhenCertificatesFailAtStart(t *testing.T) {
	f := newAccessFixture(t)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusInternalServerError)
	}))
	defer bad.Close()
	f.opt.Public.CertsURL = bad.URL + "/cdn-cgi/access/certs"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	err = Serve(context.Background(), f.opt, ln)
	if err == nil || !strings.Contains(err.Error(), f.opt.Public.CertsURL) || !strings.Contains(err.Error(), "500") {
		t.Fatalf("Serve() = %v; want certs URL and HTTP 500", err)
	}
}

func TestPublicAccessFetchesCertificatesOnceBeforeServing(t *testing.T) {
	f := newAccessFixture(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, f.opt, ln) }()
	client := &http.Client{Timeout: 2 * time.Second}
	for range 2 {
		r, err := http.NewRequest(http.MethodGet, "http://"+ln.Addr().String()+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Host = "harness.example.com"
		r.Header.Set("Cf-Access-Jwt-Assertion", f.validToken())
		var resp *http.Response
		for attempt := 0; attempt < 20; attempt++ {
			resp, err = client.Do(r)
			if err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("request = %d", resp.StatusCode)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := f.fetches.Load(); got != 1 {
		t.Fatalf("certificate fetches = %d, want startup only", got)
	}
}

func TestPublicAccessReportsLaterFetchFailureOncePerStreak(t *testing.T) {
	f := newAccessFixture(t)
	var output bytes.Buffer
	f.opt.Public.Errors = &output
	h := New(context.Background(), f.opt)
	if got := f.request(h, "/", "harness.example.com", "", f.validToken()); got.Code != 200 {
		t.Fatalf("initial request = %d", got.Code)
	}
	f.fail.Store(true)
	unknown := f.token(map[string]any{"alg": "RS256", "kid": "unknown"}, f.validClaims(), f.key)
	for range 2 {
		if got := f.request(h, "/", "harness.example.com", "", unknown); got.Code != 403 {
			t.Fatalf("failed refresh = %d", got.Code)
		}
		f.clock = f.clock.Add(61 * time.Second)
	}
	if got := strings.Count(output.String(), f.opt.Public.CertsURL); got != 1 {
		t.Fatalf("failure reports = %d, want 1: %s", got, output.String())
	}
	f.fail.Store(false)
	f.request(h, "/", "harness.example.com", "", unknown)
	f.clock = f.clock.Add(61 * time.Second)
	f.fail.Store(true)
	f.request(h, "/", "harness.example.com", "", unknown)
	if got := strings.Count(output.String(), f.opt.Public.CertsURL); got != 2 {
		t.Fatalf("failure reports after recovery = %d, want 2: %s", got, output.String())
	}
}

func TestPublicAccessKeepsKnownKeysAvailableDuringOneSlowRefresh(t *testing.T) {
	f := newAccessFixture(t)
	f.started = make(chan struct{})
	f.release = make(chan struct{})
	f.pauseAt.Store(2)
	h := New(context.Background(), f.opt)
	if got := f.request(h, "/", "harness.example.com", "", f.validToken()); got.Code != 200 {
		t.Fatalf("initial request = %d", got.Code)
	}
	newKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	newToken := f.token(map[string]any{"alg": "RS256", "kid": "new-key"}, f.validClaims(), newKey)
	knownToken := f.validToken()
	var wg sync.WaitGroup
	results := make(chan int, 9)
	wg.Add(1)
	go func() {
		defer wg.Done()
		results <- f.request(h, "/", "harness.example.com", "", newToken).Code
	}()
	<-f.started
	f.extraKey.Store(newKey)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- f.request(h, "/", "harness.example.com", "", newToken).Code
		}()
	}
	known := make(chan int, 1)
	go func() { known <- f.request(h, "/", "harness.example.com", "", knownToken).Code }()
	select {
	case code := <-known:
		if code != 200 {
			t.Errorf("known key during refresh = %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Error("known key waited for the certificate fetch")
	}
	close(f.release)
	wg.Wait()
	close(results)
	for code := range results {
		if code != http.StatusOK {
			t.Errorf("new key after refresh = %d, want 200", code)
		}
	}
	if got := f.fetches.Load(); got != 2 {
		t.Fatalf("certificate fetches = %d, want initial and one refresh", got)
	}
}

type blockingAccessWriter struct {
	started chan struct{}
	release chan struct{}
}

func (w *blockingAccessWriter) Write(p []byte) (int, error) {
	close(w.started)
	<-w.release
	return len(p), nil
}

func TestPublicAccessBlockedFailureReportDoesNotHoldKeysOrWaiters(t *testing.T) {
	f := newAccessFixture(t)
	w := &blockingAccessWriter{started: make(chan struct{}), release: make(chan struct{})}
	f.opt.Public.Errors = w
	v := &accessVerifier{opt: *f.opt.Public}
	if email, reason := v.verify(f.validToken()); reason != "" || email != "owner@example.com" {
		t.Fatalf("initial verification = %q, %q", email, reason)
	}
	f.fail.Store(true)
	unknown := f.token(map[string]any{"alg": "RS256", "kid": "unknown"}, f.validClaims(), f.key)
	fetchDone := make(chan string, 1)
	go func() {
		_, reason := v.verify(unknown)
		fetchDone <- reason
	}()
	select {
	case <-w.started:
	case <-time.After(2 * time.Second):
		t.Fatal("failure report did not start")
	}
	defer func() {
		select {
		case <-w.release:
		default:
			close(w.release)
		}
	}()
	knownToken := f.validToken()
	known := make(chan string, 1)
	go func() {
		_, reason := v.verify(knownToken)
		known <- reason
	}()
	select {
	case reason := <-known:
		if reason != "" {
			t.Errorf("cached key verification = %q", reason)
		}
	case <-time.After(2 * time.Second):
		t.Error("cached key verification waited for the error writer")
	}
	const waiters = 8
	waiterResults := make(chan string, waiters)
	for range waiters {
		go func() {
			_, reason := v.verify(unknown)
			waiterResults <- reason
		}()
	}
	for range waiters {
		select {
		case reason := <-waiterResults:
			if reason != "key fetch failed" {
				t.Errorf("waiting verification = %q, want key fetch failed", reason)
			}
		case <-time.After(2 * time.Second):
			t.Error("waiting verification waited for the error writer")
		}
	}
	close(w.release)
	if reason := <-fetchDone; reason != "key fetch failed" {
		t.Errorf("refresh verification = %q, want key fetch failed", reason)
	}
	if got := f.fetches.Load(); got != 2 {
		t.Errorf("certificate fetches = %d, want initial and one failed refresh", got)
	}
}

func TestPublicAccessLimitsEmptyCacheFailuresToOneFetchPerTenSeconds(t *testing.T) {
	f := newAccessFixture(t)
	f.fail.Store(true)
	f.opt.Public.Errors = io.Discard
	h := New(context.Background(), f.opt)
	for range 3 {
		f.request(h, "/", "harness.example.com", "", f.validToken())
	}
	if got := f.fetches.Load(); got != 1 {
		t.Fatalf("fetches without time advance = %d, want 1", got)
	}
	f.clock = f.clock.Add(9 * time.Second)
	f.request(h, "/", "harness.example.com", "", f.validToken())
	if got := f.fetches.Load(); got != 1 {
		t.Fatalf("fetches after nine seconds = %d, want 1", got)
	}
	f.clock = f.clock.Add(time.Second)
	f.request(h, "/", "harness.example.com", "", f.validToken())
	if got := f.fetches.Load(); got != 2 {
		t.Fatalf("fetches after ten seconds = %d, want 2", got)
	}
}

func TestPublicAccessWritesOnePrivateJSONLinePerRequest(t *testing.T) {
	f := newAccessFixture(t)
	h := New(context.Background(), f.opt)
	f.request(h, "/?secret=value", "harness.example.com", "", f.validToken())
	f.request(h, "/static/watch.css?secret=value", "harness.example.com", "", "")
	f.request(h, "/voice", "harness.example.com", "", f.validToken())
	path := filepath.Join(runlog.Home(), "watch-access.log")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret=value") {
		t.Fatal("query leaked into log")
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d: %s", len(lines), data)
	}
	for i, line := range lines {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if row["method"] != "GET" || row["ip"] != "198.51.100.8" {
			t.Fatalf("row = %v", row)
		}
		if _, err := time.Parse(time.RFC3339, fmt.Sprint(row["time"])); err != nil {
			t.Fatal(err)
		}
		if i == 0 && (row["status"] != float64(200) || row["path"] != "/" || row["email"] != "owner@example.com") {
			t.Fatalf("allowed row = %v", row)
		}
		if i == 1 && (row["status"] != float64(403) || row["path"] != "/static/watch.css" || row["email"] != "" || row["reason"] != "no token") {
			t.Fatalf("refused row = %v", row)
		}
		if i == 2 && (row["status"] != float64(403) || row["path"] != "/voice" || row["email"] != "" || row["reason"] == nil) {
			t.Fatalf("voice refusal row = %v", row)
		}
	}
}

func TestPublicAccessKeepsSSEFlushingThroughTheLogWriter(t *testing.T) {
	f := newAccessFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(New(ctx, f.opt))
	defer srv.Close()
	r, err := http.NewRequest(http.MethodGet, srv.URL+"/stream/index", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Host = "harness.example.com"
	r.Header.Set("Cf-Access-Jwt-Assertion", f.validToken())
	resp, err := srv.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("stream = %d %s", resp.StatusCode, b)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "retry: 2000\n" {
		t.Fatalf("first flushed line = %q, %v", line, err)
	}
}

func TestStreamSessionExpiry(t *testing.T) {
	f := newAccessFixture(t)
	f.clock = time.Now()
	f.opt.Public.Now = time.Now
	claims := f.validClaims()
	claims["exp"] = f.clock.Unix() - 57
	expiry := time.Unix(claims["exp"].(int64), 0).Add(60 * time.Second)
	token := f.token(map[string]any{"alg": "RS256", "kid": "test-key"}, claims, f.key)

	for _, public := range []bool{true, false} {
		name := "local"
		opt := Options{Poll: time.Hour}
		if public {
			name = "public"
			opt = f.opt
			opt.Poll = time.Hour
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			r := httptest.NewRequest(http.MethodGet, "/stream/index", nil).WithContext(ctx)
			if public {
				r.Host = "harness.example.com"
				r.Header.Set("Cf-Access-Jwt-Assertion", token)
			} else {
				r.Host = "localhost"
			}
			w := httptest.NewRecorder()
			started := time.Now()
			New(context.Background(), opt).ServeHTTP(w, r)
			body := w.Body.String()
			if public {
				if !strings.Contains(body, "event: end\ndata: session expired\n") || time.Now().Before(expiry) || time.Since(started) >= 4*time.Second {
					t.Fatalf("public stream did not expire: %s", body)
				}
				row := lastAccessRow(t)
				if row["stream"] != "end" || row["reason"] != "session expired" {
					t.Fatalf("stream end row = %v", row)
				}
			} else if strings.Contains(body, "session expired") || ctx.Err() == nil {
				t.Fatalf("local stream ended early: %s", body)
			}
		})
	}
}

func lastAccessRow(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(runlog.Home(), "watch-access.log"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var row map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &row); err != nil {
		t.Fatal(err)
	}
	return row
}

func TestPublicAccessLogsAStreamRowAtStartAndAtEnd(t *testing.T) {
	f := newAccessFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(New(ctx, f.opt))
	defer srv.Close()
	r, err := http.NewRequest(http.MethodGet, srv.URL+"/stream/index", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Host = "harness.example.com"
	r.Header.Set("Cf-Access-Jwt-Assertion", f.validToken())
	resp, err := srv.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	row := waitAccessRow(t)
	if row["stream"] != "start" || row["status"] != float64(200) || row["path"] != "/stream/index" || row["email"] != "owner@example.com" {
		t.Fatalf("row while the stream is open = %v", row)
	}
	resp.Body.Close()
	path := filepath.Join(runlog.Home(), "watch-access.log")
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(lines) == 2 {
			var end map[string]any
			if err := json.Unmarshal([]byte(lines[1]), &end); err != nil {
				t.Fatal(err)
			}
			if end["stream"] != "end" || end["status"] != float64(200) || end["path"] != "/stream/index" {
				t.Fatalf("row after the stream ends = %v", end)
			}
			return
		}
		if len(lines) > 2 || time.Now().After(deadline) {
			t.Fatalf("log after the stream ends: %s", data)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitAccessRow returns the last access row once one exists: a stream's start
// row follows its first flush, so the browser may read before it is written.
func waitAccessRow(t *testing.T) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if data, err := os.ReadFile(filepath.Join(runlog.Home(), "watch-access.log")); err == nil && len(bytes.TrimSpace(data)) > 0 {
			return lastAccessRow(t)
		}
		if time.Now().After(deadline) {
			t.Fatal("no access row")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPublicAccessSendsAStreamsFirstByteWhileTheLogIsBusy(t *testing.T) {
	f := newAccessFixture(t)
	s := &server{opt: f.opt}
	done := make(chan struct{})
	h := s.publicLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "retry: 2000\n\n")
		_ = http.NewResponseController(w).Flush()
		<-done
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()
	defer close(done)
	s.logMu.Lock()
	locked := true
	defer func() {
		if locked {
			s.logMu.Unlock()
		}
	}()
	line := make(chan string, 1)
	go func() {
		resp, err := srv.Client().Get(srv.URL + "/stream/index")
		if err != nil {
			line <- err.Error()
			return
		}
		defer resp.Body.Close()
		l, _ := bufio.NewReader(resp.Body).ReadString('\n')
		line <- l
	}()
	select {
	case l := <-line:
		if l != "retry: 2000\n" {
			t.Fatalf("first line = %q", l)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream's first byte waited for the access log")
	}
	locked = false
	s.logMu.Unlock()
	if row := waitAccessRow(t); row["stream"] != "start" || row["path"] != "/stream/index" {
		t.Fatalf("start row = %v", row)
	}
}

func TestPublicGuardCalledWithoutTheLogWriterRefusesInsteadOfPanicking(t *testing.T) {
	f := newAccessFixture(t)
	s := &server{opt: f.opt}
	h := s.guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("guard passed a request through without the access log writer")
	}), nil, "")
	w := f.request(h, "/", "harness.example.com", "", f.validToken())
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d %s, want 500", w.Code, w.Body)
	}
}

// streamAs reads what a live route sends in its first moments, then hangs up.
func (f *accessFixture) streamAs(h http.Handler, path, token string) string {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8089"+path, nil).WithContext(ctx)
	r.Host = "harness.example.com"
	r.RemoteAddr = "127.0.0.1:5678"
	r.Header.Set("Cf-Connecting-IP", "198.51.100.8")
	r.Header.Set("Cf-Access-Jwt-Assertion", token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Body.String()
}

func (f *accessFixture) tokenFor(email string) string {
	claims := f.validClaims()
	claims["email"] = email
	return f.token(map[string]any{"alg": "RS256", "kid": "test-key"}, claims, f.key)
}

func TestAStrangerGetsNothingFromTheFlowJSONTheLiveIndexOrAda(t *testing.T) {
	f := newAccessFixture(t)
	t.Setenv("HRN_OWNER", "local:test")
	runID := saveRun(t, "fake", "polish the zebra module", nil)
	f.opt.OwnerEmails = []string{"owner@example.com"}
	f.opt.Poll = 5 * time.Millisecond
	h := New(context.Background(), f.opt)
	stranger, mine := f.tokenFor("other@example.com"), f.tokenFor("owner@example.com")
	if resp := f.request(h, "/flow/"+runID+"/json", "harness.example.com", "", stranger); resp.Code != http.StatusNotFound {
		t.Fatalf("flow JSON for a stranger: %d", resp.Code)
	}
	if resp := f.request(h, "/flow/"+runID+"/json", "harness.example.com", "", mine); resp.Code != http.StatusOK {
		t.Fatalf("flow JSON for the owner: %d", resp.Code)
	}
	if body := f.streamAs(h, "/stream/index", mine); !strings.Contains(body, runID) {
		t.Fatalf("control: the owner's live index must list the run: %s", body)
	}
	if body := f.streamAs(h, "/stream/index", stranger); strings.Contains(body, runID) || strings.Contains(body, "zebra") {
		t.Errorf("the live index showed a stranger the owner's run: %s", body)
	}
}

func TestAdaOnlyWatchesWhatHerViewerMayRead(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:test")
	runID := saveRun(t, "fake", "polish the zebra module", &agent.Result{Status: agent.StatusOK})
	s := &server{opt: Options{Runs: 50, Public: &PublicAccess{}, OwnerEmails: []string{"owner@example.com"}}}
	as := func(email string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/stream/bot", nil)
		return r.WithContext(context.WithValue(r.Context(), accessContextKey{}, &accessWriter{email: email}))
	}
	if _, ok := s.newBotWatcher(as("owner@example.com")).look().find(runID); !ok {
		t.Fatal("control: Ada must watch the owner's run")
	}
	if snap := s.newBotWatcher(as("other@example.com")).look(); len(snap.items) != 0 {
		t.Fatalf("Ada watched %d records for a stranger", len(snap.items))
	}
}

func TestWithoutOwnerEmailsEveryVerifiedVisitorIsTheLocalUser(t *testing.T) {
	f := newAccessFixture(t)
	t.Setenv("HRN_OWNER", "local:test")
	runID := saveRun(t, "fake", "mine", &agent.Result{Status: agent.StatusOK})
	h := New(context.Background(), f.opt)
	for _, path := range []string{"/run/" + runID, "/flow/" + runID} {
		if resp := f.request(h, path, "harness.example.com", "", f.tokenFor("anyone@example.com")); resp.Code != http.StatusOK {
			t.Errorf("%s: %d, want the behaviour before ownership: every verified visitor reads local records", path, resp.Code)
		}
	}
}
