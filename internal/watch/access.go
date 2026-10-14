package watch

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cristiangirlea/harness/internal/runlog"
)

// PublicAccess enables Cloudflare Access verification for a loopback-only
// watch server. CertsURL, Client, Now and Errors are test seams; leave them empty in
// the CLI so certificates come from the named team with a 10-second timeout.
type PublicAccess struct {
	Origin   string
	Team     string
	Audience string
	CertsURL string
	Client   *http.Client
	Now      func() time.Time
	Errors   io.Writer
}

// ParsePublicAccess validates the three flags before a listener is opened.
func ParsePublicAccess(origin, team, audience string) (*PublicAccess, error) {
	if origin == "" || team == "" || audience == "" {
		return nil, errors.New("--public, --access-team and --access-aud must be given together")
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" || origin != "https://"+u.Host || u.Port() != "" && u.Port() != "443" {
		return nil, errors.New("--public must be an HTTPS origin without a path, query or userinfo")
	}
	validHost := func(host string) bool {
		if host == "" || len(host) > 253 {
			return false
		}
		for _, label := range strings.Split(host, ".") {
			if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return false
			}
			for _, c := range label {
				if c != '-' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
					return false
				}
			}
		}
		return true
	}
	if !validHost(u.Hostname()) || !validHost(team) || !strings.HasSuffix(strings.ToLower(team), ".cloudflareaccess.com") {
		return nil, errors.New("--public needs a DNS host and --access-team needs a cloudflareaccess.com team host")
	}
	if strings.ContainsAny(audience, " \t\r\n") {
		return nil, errors.New("--access-aud must be one audience tag")
	}
	return &PublicAccess{Origin: "https://" + strings.ToLower(u.Hostname()), Team: strings.ToLower(team), Audience: audience}, nil
}

type accessVerifier struct {
	opt              PublicAccess
	mu               sync.Mutex
	keys             map[string]*rsa.PublicKey
	lastUnknownFetch time.Time
	lastEmptyFailure time.Time
	lastFetchErr     error
	fetching         chan struct{}
	failureReported  bool
}

func (v *accessVerifier) now() time.Time {
	if v.opt.Now != nil {
		return v.opt.Now()
	}
	return time.Now()
}

func (v *accessVerifier) verify(token string) (string, string) {
	if token == "" {
		return "", "no token"
	}
	if len(token) > 16384 {
		return "", "bad token"
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", "bad token"
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", "bad token"
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if json.Unmarshal(hb, &header) != nil || header.Alg != "RS256" {
		return "", "bad alg"
	}
	if header.Kid == "" {
		return "", "unknown kid"
	}
	key, err := v.key(header.Kid)
	if err != nil {
		return "", "key fetch failed"
	}
	if key == nil {
		return "", "unknown kid"
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", "bad signature"
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig) != nil {
		return "", "bad signature"
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "bad claims"
	}
	var claims struct {
		Audience  json.RawMessage `json:"aud"`
		Issuer    string          `json:"iss"`
		Expiry    json.Number     `json:"exp"`
		NotBefore json.Number     `json:"nbf"`
		IssuedAt  json.Number     `json:"iat"`
		Email     string          `json:"email"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return "", "bad claims"
	}
	if claims.Issuer != "https://"+v.opt.Team {
		return "", "wrong iss"
	}
	var aud string
	if json.Unmarshal(claims.Audience, &aud) == nil {
		if aud != v.opt.Audience {
			return "", "wrong aud"
		}
	} else {
		var audiences []string
		if json.Unmarshal(claims.Audience, &audiences) != nil {
			return "", "wrong aud"
		}
		found := false
		for _, a := range audiences {
			if a == v.opt.Audience {
				found = true
			}
		}
		if !found {
			return "", "wrong aud"
		}
	}
	now := v.now().Unix()
	exp, err := strconv.ParseInt(string(claims.Expiry), 10, 64)
	if err != nil || exp <= now-60 {
		return "", "expired"
	}
	for _, claim := range []json.Number{claims.NotBefore, claims.IssuedAt} {
		if claim == "" {
			continue
		}
		n, err := strconv.ParseInt(string(claim), 10, 64)
		if err != nil {
			return "", "bad claims"
		}
		if n > now+60 {
			return "", "not yet valid"
		}
	}
	return claims.Email, ""
}

func (v *accessVerifier) key(kid string) (*rsa.PublicKey, error) {
	for {
		v.mu.Lock()
		if key := v.keys[kid]; key != nil {
			v.mu.Unlock()
			return key, nil
		}
		if v.fetching != nil {
			wait := v.fetching
			v.mu.Unlock()
			<-wait
			continue
		}
		now := v.now()
		if v.keys == nil {
			if !v.lastEmptyFailure.IsZero() && now.Sub(v.lastEmptyFailure) < 10*time.Second {
				err := v.lastFetchErr
				v.mu.Unlock()
				return nil, err
			}
		} else {
			if !v.lastUnknownFetch.IsZero() && now.Sub(v.lastUnknownFetch) < time.Minute {
				err := v.lastFetchErr
				v.mu.Unlock()
				return nil, err
			}
			v.lastUnknownFetch = now
		}
		v.fetching = make(chan struct{})
		v.mu.Unlock()

		keys, err := v.fetch()
		v.mu.Lock()
		if err != nil {
			v.lastFetchErr = err
			if v.keys == nil {
				v.lastEmptyFailure = v.now()
			}
			if !v.failureReported {
				out := v.opt.Errors
				if out == nil {
					out = os.Stderr
				}
				fmt.Fprintf(out, "hrn watch: Cloudflare Access keys at %s: %v\n", v.certsURL(), err)
				v.failureReported = true
			}
		} else {
			v.keys = keys
			v.lastFetchErr = nil
			v.lastEmptyFailure = time.Time{}
			v.failureReported = false
			if keys[kid] == nil {
				v.lastUnknownFetch = now
			}
		}
		close(v.fetching)
		v.fetching = nil
		key := v.keys[kid]
		v.mu.Unlock()
		return key, err
	}
}

func (v *accessVerifier) certsURL() string {
	endpoint := v.opt.CertsURL
	if endpoint == "" {
		endpoint = "https://" + v.opt.Team + "/cdn-cgi/access/certs"
	}
	return endpoint
}

func (v *accessVerifier) fetch() (map[string]*rsa.PublicKey, error) {
	client := v.opt.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Get(v.certsURL())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("certificates: HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 1<<20 {
		return nil, errors.New("certificates too large")
	}
	var jwks struct {
		Keys []struct{ Kid, Kty, N, E string } `json:"keys"`
	}
	if err := json.Unmarshal(b, &jwks); err != nil {
		return nil, err
	}
	keys := make(map[string]*rsa.PublicKey)
	for _, k := range jwks.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		n, errN := base64.RawURLEncoding.DecodeString(k.N)
		e, errE := base64.RawURLEncoding.DecodeString(k.E)
		if errN != nil || errE != nil || len(n) == 0 || len(e) == 0 || len(e) > 4 {
			continue
		}
		exponent := new(big.Int).SetBytes(e).Int64()
		if exponent < 3 || exponent > int64(^uint(0)>>1) || exponent%2 == 0 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exponent)}
	}
	if len(keys) == 0 {
		return nil, errors.New("certificates: no RSA keys")
	}
	return keys, nil
}

type accessWriter struct {
	http.ResponseWriter
	status int
	email  string
	reason string
}

func (w *accessWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *accessWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *accessWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (s *server) publicLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aw := &accessWriter{ResponseWriter: w}
		defer func() {
			if aw.status == 0 {
				aw.status = http.StatusOK
			}
			if aw.status == http.StatusForbidden {
				aw.email = ""
				if aw.reason == "" {
					aw.reason = "forbidden"
				}
			}
			row := struct {
				Time   string `json:"time"`
				Email  string `json:"email"`
				Method string `json:"method"`
				Path   string `json:"path"`
				Status int    `json:"status"`
				IP     string `json:"ip"`
				Reason string `json:"reason,omitempty"`
			}{time.Now().UTC().Format(time.RFC3339), aw.email, r.Method, r.URL.Path, aw.status, r.Header.Get("Cf-Connecting-IP"), aw.reason}
			b, err := json.Marshal(row)
			if err != nil {
				return
			}
			s.logMu.Lock()
			defer s.logMu.Unlock()
			if err := os.MkdirAll(runlog.Home(), 0700); err != nil {
				return
			}
			f, err := os.OpenFile(filepath.Join(runlog.Home(), "watch-access.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				return
			}
			defer f.Close()
			_, _ = f.Write(append(b, '\n'))
		}()
		next.ServeHTTP(aw, r)
	})
}
