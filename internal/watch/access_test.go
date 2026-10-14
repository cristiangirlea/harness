package watch

import (
	"bufio"
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/runlog"
)

type accessFixture struct {
	t        *testing.T
	key      *rsa.PrivateKey
	extraKey atomic.Pointer[rsa.PrivateKey]
	certs    *httptest.Server
	fetches  atomic.Int32
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
		f.fetches.Add(1)
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
	for _, tc := range []struct{ name, host, origin, token string }{
		{"missing token", "harness.example.com", "", ""},
		{"wrong host", "other.example", "", token},
		{"loopback host", "127.0.0.1:8089", "", token},
		{"wrong origin scheme", "harness.example.com", "http://harness.example.com", token},
		{"wrong origin host", "harness.example.com", "https://other.example", token},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := f.request(h, "/", tc.host, tc.origin, tc.token); got.Code != 403 {
				t.Fatalf("status = %d", got.Code)
			}
		})
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
		{"cross-site GET navigation", http.MethodGet, "harness.example.com", "", token, map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "cross-site"}, 200},
		{"cross-site iframe navigation relies on frame headers", http.MethodGet, "harness.example.com", "", token, map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "iframe", "Sec-Fetch-Site": "cross-site"}, 200},
		{"null-origin GET navigation", http.MethodGet, "harness.example.com", "null", token, map[string]string{"Sec-Fetch-Mode": "navigate"}, 200},
		{"cross-site HEAD navigation", http.MethodHead, "harness.example.com", "", token, map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "cross-site"}, 200},
		{"navigation without Access token", http.MethodGet, "harness.example.com", "", "", map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "cross-site"}, 403},
		{"cross-site CORS GET", http.MethodGet, "harness.example.com", "", token, map[string]string{"Sec-Fetch-Mode": "cors", "Sec-Fetch-Site": "cross-site"}, 403},
		{"cross-site POST navigation", http.MethodPost, "harness.example.com", "", token, map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "cross-site", "X-Hrn-Watch-Key": "k3y"}, 403},
		{"navigation with wrong Host", http.MethodGet, "other.example", "", token, map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "cross-site"}, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := f.requestWithHeaders(h, tc.method, "/", tc.host, tc.origin, tc.token, tc.headers)
			if got.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", got.Code, tc.want, got.Body)
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := f.request(h, "/", "harness.example.com", "", f.token(tc.header, tc.claims, tc.key))
			if w.Code != 403 || strings.TrimSpace(w.Body.String()) != "Cloudflare Access login required" {
				t.Fatalf("status %d body %q", w.Code, w.Body.String())
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
