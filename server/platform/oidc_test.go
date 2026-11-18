package platform

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var oidcTestKeys = sync.OnceValues(func() (*rsa.PrivateKey, *ecdsa.PrivateKey) {
	r, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	e, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	return r, e
})

func oidcEncode(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }

func oidcRSAKey(kid string, key *rsa.PublicKey) map[string]any {
	return map[string]any{"kid": kid, "kty": "RSA", "n": oidcEncode(key.N.Bytes()), "e": oidcEncode(big.NewInt(int64(key.E)).Bytes())}
}

type oidcFixture struct {
	issuer     string
	now        time.Time
	rsa        *rsa.PrivateKey
	ec         *ecdsa.PrivateKey
	discovery  atomic.Value
	keys       atomic.Value
	fetches    atomic.Int32
	documents  atomic.Int32
	beforeKeys atomic.Value
}

func newOIDCFixture(t *testing.T) *oidcFixture {
	t.Helper()
	r, e := oidcTestKeys()
	f := &oidcFixture{now: time.Unix(1_800_000_000, 0), rsa: r, ec: e}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/issuer/.well-known/openid-configuration":
			f.documents.Add(1)
			raw := f.discovery.Load().(string)
			if raw == "redirect" {
				http.Redirect(w, r, "/keys", http.StatusFound)
				return
			}
			_, _ = io.WriteString(w, raw)
		case "/keys":
			f.fetches.Add(1)
			if before, ok := f.beforeKeys.Load().(func()); ok {
				before()
			}
			raw := f.keys.Load().(string)
			if raw == "redirect" {
				http.Redirect(w, r, "/issuer/.well-known/openid-configuration", http.StatusFound)
				return
			}
			_, _ = io.WriteString(w, raw)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	f.issuer = s.URL + "/issuer/"
	f.discovery.Store(string(oidcJSON(t, map[string]string{"issuer": f.issuer, "jwks_uri": s.URL + "/keys"})))
	f.setKeys(t, oidcRSAKey("rsa", &r.PublicKey), map[string]any{
		"kid": "ec", "kty": "EC", "crv": "P-256", "alg": "ES256", "use": "sig",
		"x": oidcEncode(e.X.FillBytes(make([]byte, 32))), "y": oidcEncode(e.Y.FillBytes(make([]byte, 32))),
	})
	return f
}

func oidcJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (f *oidcFixture) setKeys(t *testing.T, keys ...map[string]any) {
	t.Helper()
	f.keys.Store(string(oidcJSON(t, map[string]any{"keys": keys})))
}

func (f *oidcFixture) verifier(t *testing.T) *OIDCVerifier {
	t.Helper()
	v, err := NewOIDCVerifier(OIDCConfig{
		Issuer: f.issuer, Audience: "control", OperatorClaim: "roles", OperatorValue: "operator", TenantClaim: "tenants",
	})
	if err != nil {
		t.Fatal(err)
	}
	v.now = func() time.Time { return f.now }
	return v
}

func (f *oidcFixture) claims() map[string]any {
	return map[string]any{"iss": f.issuer, "aud": "control", "sub": "person", "exp": f.now.Unix() + 300}
}

func (f *oidcFixture) sign(t *testing.T, header, claims any, alg string) string {
	t.Helper()
	encode := func(value any) string {
		if raw, ok := value.(string); ok {
			return oidcEncode([]byte(raw))
		}
		return oidcEncode(oidcJSON(t, value))
	}
	return f.signSegments(t, encode(header)+"."+encode(claims), alg)
}

func (f *oidcFixture) signSegments(t *testing.T, input, alg string) string {
	t.Helper()
	digest := sha256.Sum256([]byte(input))
	var sig []byte
	var err error
	switch alg {
	case "RS256":
		sig, err = rsa.SignPKCS1v15(rand.Reader, f.rsa, crypto.SHA256, digest[:])
	case "ES256":
		var r, s *big.Int
		r, s, err = ecdsa.Sign(rand.Reader, f.ec, digest[:])
		if err == nil {
			sig = make([]byte, 64)
			r.FillBytes(sig[:32])
			s.FillBytes(sig[32:])
		}
	case "HS256":
		mac := hmac.New(sha256.New, x509.MarshalPKCS1PublicKey(&f.rsa.PublicKey))
		_, _ = mac.Write([]byte(input))
		sig = mac.Sum(nil)
	case "none":
	default:
		t.Fatalf("unexpected test signing algorithm %q", alg)
	}
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + oidcEncode(sig)
}

func oidcHeader(alg, kid string) map[string]any { return map[string]any{"alg": alg, "kid": kid} }

func TestOIDCIdentities(t *testing.T) {
	f := newOIDCFixture(t)
	v := f.verifier(t)
	for _, tc := range []struct {
		name, alg, kid      string
		aud, roles, tenants any
		operator            bool
		wantTenants         []string
	}{
		{"RSA operator and tenant strings", "RS256", "rsa", "control", "operator", "first", true, []string{"first"}},
		{"EC operator and tenant arrays", "ES256", "ec", []string{"another", "control"}, []string{"reader", "operator"}, []any{"first", false, "second"}, true, []string{"first", "second"}},
		{"tenant only", "RS256", "rsa", "control", []any{"operator", 7}, "first", false, []string{"first"}},
		{"boolean operator", "RS256", "rsa", "control", true, nil, false, nil},
		{"object operator", "RS256", "rsa", "control", map[string]string{"role": "operator"}, nil, false, nil},
		{"nested operator array", "RS256", "rsa", "control", []any{"operator", []string{"operator"}}, nil, false, nil},
		{"object tenant", "RS256", "rsa", "control", nil, map[string]string{"tenant": "first"}, false, nil},
		{"numeric tenant", "RS256", "rsa", "control", nil, 7, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := f.claims()
			claims["aud"], claims["roles"], claims["tenants"] = tc.aud, tc.roles, tc.tenants
			got, err := v.Verify(context.Background(), f.sign(t, oidcHeader(tc.alg, tc.kid), claims, tc.alg))
			want := Identity{Subject: "person", Issuer: f.issuer, Operator: tc.operator, Tenants: tc.wantTenants}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("identity = %+v, %v, want %+v", got, err, want)
			}
		})
	}
	if f.fetches.Load() != 1 || f.documents.Load() != 1 {
		t.Fatal("cached keys caused another fetch")
	}
	claims := f.claims()
	claims["tenants"] = strings.Fields(strings.Repeat("t ", 300))
	got, err := v.Verify(context.Background(), f.sign(t, oidcHeader("RS256", "rsa"), claims, "RS256"))
	if err != nil || len(got.Tenants) != 256 {
		t.Fatalf("tenant count = %d, %v", len(got.Tenants), err)
	}
}

func TestOIDCClaimRefusalsAndLeeway(t *testing.T) {
	f := newOIDCFixture(t)
	v := f.verifier(t)
	now := f.now.Unix()
	for _, tc := range []struct {
		name, claim string
		value       any
		valid       bool
	}{
		{"exact issuer", "iss", strings.TrimSuffix(f.issuer, "/"), false},
		{"wrong audience", "aud", "other", false},
		{"wrong audience array", "aud", []string{"other"}, false},
		{"mixed audience array", "aud", []any{"control", nil}, false},
		{"expired", "exp", now - 61, false},
		{"expiry boundary", "exp", now - 60, false},
		{"within leeway", "exp", now - 59, true},
		{"future nbf", "nbf", now + 61, false},
		{"future iat", "iat", now + 61, false},
		{"nbf boundary", "nbf", now + 60, true},
		{"iat boundary", "iat", now + 60, true},
		{"missing expiry", "exp", nil, false},
		{"string expiry", "exp", "1800000300", false},
		{"out of range expiry", "exp", json.Number("1e999"), false},
		{"string nbf", "nbf", "1800000000", false},
		{"non-number iat", "iat", false, false},
		{"missing subject", "sub", nil, false},
		{"empty subject", "sub", "", false},
		{"long subject", "sub", strings.Repeat("x", 257), false},
		{"256 character subject", "sub", strings.Repeat("x", 256), true},
		{"subject with newline", "sub", "person\nname", false},
		{"subject with DEL", "sub", "person\x7fname", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := f.claims()
			if tc.value == nil {
				delete(claims, tc.claim)
			} else {
				claims[tc.claim] = tc.value
			}
			_, err := v.Verify(context.Background(), f.sign(t, oidcHeader("RS256", "rsa"), claims, "RS256"))
			if tc.valid && err != nil || !tc.valid && err != ErrOIDCInvalid {
				t.Fatalf("Verify = %v, valid = %t", err, tc.valid)
			}
		})
	}
}

func TestOIDCMalformedTokensNeverFetchKeys(t *testing.T) {
	f := newOIDCFixture(t)
	v := f.verifier(t)
	claims := f.claims()
	header := oidcHeader("RS256", "rsa")
	good := f.sign(t, header, claims, "RS256")
	segments := strings.Split(good, ".")
	ecPrefix, _, _ := strings.Cut(f.sign(t, oidcHeader("ES256", "ec"), claims, "ES256"), ".")
	payload := string(oidcJSON(t, claims))
	headerJSON := string(oidcJSON(t, header))
	withTenant := strings.TrimSuffix(payload, "}") + `,"tenants":"first"}`
	standard := base64.RawStdEncoding.EncodeToString([]byte(strings.TrimSuffix(payload, "}") + `,"extra":"~~~~~"}`))
	if !strings.ContainsAny(standard, "+/") {
		t.Fatal("standard base64 fixture lacks a standard-only character")
	}
	padded := payload
	for len(padded)%3 != 1 {
		padded += " "
	}
	noncanonical := oidcEncode([]byte(padded))
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, noncanonical[len(noncanonical)-1])
	noncanonical = noncanonical[:len(noncanonical)-1] + string(alphabet[last+1])
	bad := map[string]string{
		"oversize":               strings.Repeat("x", 8193),
		"two segments":           segments[0] + "." + segments[1],
		"four segments":          good + ".x",
		"padding":                segments[0] + "=." + segments[1] + "." + segments[2],
		"line break":             good + "\n",
		"none":                   f.sign(t, oidcHeader("none", "rsa"), claims, "none"),
		"HMAC with public key":   f.sign(t, oidcHeader("HS256", "rsa"), claims, "HS256"),
		"non-JSON header":        f.sign(t, "invalid JSON", claims, "RS256"),
		"non-JSON payload":       f.sign(t, header, "invalid JSON", "RS256"),
		"duplicate algorithm":    f.sign(t, strings.TrimSuffix(headerJSON, "}")+`,"alg":"RS256"}`, claims, "RS256"),
		"duplicate subject":      f.sign(t, header, strings.TrimSuffix(payload, "}")+`,"sub":"person"}`, "RS256"),
		"duplicate tenant claim": f.sign(t, header, strings.TrimSuffix(withTenant, "}")+`,"tenants":"first"}`, "RS256"),
		"trailing header data":   f.sign(t, headerJSON+" {}", claims, "RS256"),
		"trailing payload data":  f.sign(t, header, payload+" {}", "RS256"),
		"standard base64":        f.signSegments(t, segments[0]+"."+standard, "RS256"),
		"non-zero trailing bits": f.signSegments(t, segments[0]+"."+noncanonical, "RS256"),
		"missing kid":            f.sign(t, map[string]any{"alg": "RS256"}, claims, "RS256"),
		"empty kid":              f.sign(t, oidcHeader("RS256", ""), claims, "RS256"),
		"long kid":               f.sign(t, oidcHeader("RS256", strings.Repeat("x", 257)), claims, "RS256"),
		"short EC signature":     ecPrefix + "." + segments[1] + ".AQ",
	}
	for _, name := range []string{"jku", "jwk", "x5u", "x5c", "crit"} {
		h := oidcHeader("RS256", "rsa")
		h[name] = nil
		bad[name] = f.sign(t, h, claims, "RS256")
	}
	for name, raw := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := v.Verify(context.Background(), raw); err != ErrOIDCInvalid {
				t.Fatalf("Verify = %v", err)
			}
		})
	}
	if f.fetches.Load() != 0 || f.documents.Load() != 0 {
		t.Fatal("malformed token caused network access")
	}
}

func TestOIDCRejectsInvalidP256Keys(t *testing.T) {
	f := newOIDCFixture(t)
	raw := f.sign(t, oidcHeader("ES256", "ec"), f.claims(), "ES256")
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"point off curve", func(k map[string]any) {
			k["x"], k["y"] = oidcEncode(make([]byte, 32)), oidcEncode(make([]byte, 32))
		}},
		{"wrong curve name", func(k map[string]any) { k["crv"] = "P-384" }},
		{"oversized x", func(k map[string]any) { k["x"] = oidcEncode(f.ec.X.FillBytes(make([]byte, 33))) }},
		{"oversized y", func(k map[string]any) { k["y"] = oidcEncode(f.ec.Y.FillBytes(make([]byte, 33))) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := map[string]any{
				"kid": "ec", "kty": "EC", "crv": "P-256",
				"x": oidcEncode(f.ec.X.FillBytes(make([]byte, 32))),
				"y": oidcEncode(f.ec.Y.FillBytes(make([]byte, 32))),
			}
			tc.change(key)
			f.setKeys(t, key)
			if _, err := f.verifier(t).Verify(context.Background(), raw); err != ErrOIDCInvalid {
				t.Fatalf("Verify = %v", err)
			}
		})
	}
}

func TestOIDCRejectsInvalidES256Signatures(t *testing.T) {
	f := newOIDCFixture(t)
	v := f.verifier(t)
	header, claims := oidcHeader("ES256", "ec"), f.claims()
	token := f.sign(t, header, claims, "ES256")
	if _, err := v.Verify(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	sigStart := strings.LastIndexByte(token, '.') + 1
	prefix, encoded := token[:sigStart], token[sigStart:]
	claims["sub"] = "another person"
	other := f.sign(t, header, claims, "ES256")
	otherSignature := other[strings.LastIndexByte(other, '.')+1:]
	for _, tc := range []struct {
		name   string
		change func([]byte)
	}{
		{"signature for other claims", func(sig []byte) { copy(sig, oidcSegment(otherSignature)) }},
		{"zero r", func(sig []byte) { clear(sig[:32]) }},
		{"r equals curve order", func(sig []byte) { f.ec.Params().N.FillBytes(sig[:32]) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sig := oidcSegment(encoded)
			tc.change(sig)
			if _, err := v.Verify(context.Background(), prefix+oidcEncode(sig)); err != ErrOIDCInvalid {
				t.Fatalf("Verify = %v", err)
			}
		})
	}
}

func TestOIDCRejectsUnusableKeysAndSignatures(t *testing.T) {
	f := newOIDCFixture(t)
	short, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, alg, kid string
		key            map[string]any
	}{
		{"RSA with EC key", "RS256", "ec", nil},
		{"EC with RSA key", "ES256", "rsa", nil},
		{"unknown key", "RS256", "unknown", nil},
		{"bad signature", "RS256", "rsa", nil},
		{"short RSA modulus", "RS256", "rsa", oidcRSAKey("rsa", &short.PublicKey)},
		{"bad public exponent", "RS256", "rsa", oidcRSAKey("rsa", &rsa.PublicKey{N: f.rsa.N, E: 2})},
		{"encryption key", "RS256", "rsa", oidcRSAKey("rsa", &f.rsa.PublicKey)},
		{"different key algorithm", "RS256", "rsa", oidcRSAKey("rsa", &f.rsa.PublicKey)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := f.sign(t, oidcHeader(tc.alg, tc.kid), f.claims(), tc.alg)
			if tc.name == "bad signature" {
				prefix, sig, _ := strings.Cut(raw, ".")
				payload, sig, _ := strings.Cut(sig, ".")
				bytes, err := base64.RawURLEncoding.DecodeString(sig)
				if err != nil {
					t.Fatal(err)
				}
				bytes[0] ^= 1
				raw = prefix + "." + payload + "." + oidcEncode(bytes)
			}
			if tc.key != nil {
				if tc.name == "encryption key" {
					tc.key["use"] = "enc"
				}
				if tc.name == "different key algorithm" {
					tc.key["alg"] = "RS512"
				}
				f.setKeys(t, tc.key)
			}
			if _, err := f.verifier(t).Verify(context.Background(), raw); err != ErrOIDCInvalid {
				t.Fatalf("Verify = %v", err)
			}
		})
	}
}

func TestOIDCRotationAndSharedRefreshLimit(t *testing.T) {
	f := newOIDCFixture(t)
	v := f.verifier(t)
	verify := func(kid string) error {
		_, err := v.Verify(context.Background(), f.sign(t, oidcHeader("RS256", kid), f.claims(), "RS256"))
		return err
	}
	if err := verify("rsa"); err != nil {
		t.Fatal(err)
	}
	f.setKeys(t, oidcRSAKey("rotated", &f.rsa.PublicKey))
	if err := verify("rotated"); err != nil || f.fetches.Load() != 2 {
		t.Fatalf("rotation: requests = %d, error = %v", f.fetches.Load(), err)
	}
	f.now = f.now.Add(61 * time.Second)
	var wg sync.WaitGroup
	for i := range 50 {
		raw := f.sign(t, oidcHeader("RS256", "unknown"+string(rune('a'+i))), f.claims(), "RS256")
		wg.Go(func() {
			if _, err := v.Verify(context.Background(), raw); err != ErrOIDCInvalid {
				t.Errorf("unknown key: %v", err)
			}
		})
	}
	wg.Wait()
	if f.fetches.Load() != 3 {
		t.Fatalf("unknown keys caused %d refreshes, want one", f.fetches.Load()-2)
	}
	f.now = f.now.Add(61 * time.Second)
	f.keys.Store("invalid JSON")
	if err := verify("unknown"); err != ErrOIDCUnavailable || f.fetches.Load() != 4 {
		t.Fatalf("failed refresh: requests = %d, error = %v", f.fetches.Load(), err)
	}
	if err := verify("unknown"); err == nil || f.fetches.Load() != 4 {
		t.Fatalf("failed refresh wasn't limited: requests = %d, error = %v", f.fetches.Load(), err)
	}
	if err := verify("rotated"); err != nil {
		t.Fatalf("failed refresh lost cached keys: %v", err)
	}
}

func TestOIDCCanceledRequestStillLoadsKeys(t *testing.T) {
	f := newOIDCFixture(t)
	v := f.verifier(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	unknown := f.sign(t, oidcHeader("RS256", "unknown"), f.claims(), "RS256")
	known := f.sign(t, oidcHeader("RS256", "rsa"), f.claims(), "RS256")
	if _, err := v.Verify(ctx, unknown); err != ErrOIDCInvalid {
		t.Errorf("unknown key after fetch: %v, want invalid token", err)
	}
	if _, err := v.Verify(context.Background(), known); err != nil {
		t.Errorf("cached key after canceled request: %v", err)
	}
	if f.documents.Load() != 1 || f.fetches.Load() != 1 {
		t.Fatalf("discovery requests = %d, key set requests = %d, want one each", f.documents.Load(), f.fetches.Load())
	}
}

func TestOIDCWaiterStopsWaitingWhenItsRequestIsCanceled(t *testing.T) {
	f := newOIDCFixture(t)
	v := f.verifier(t)
	known := f.sign(t, oidcHeader("RS256", "rsa"), f.claims(), "RS256")
	unknown := f.sign(t, oidcHeader("RS256", "unknown"), f.claims(), "RS256")
	if _, err := v.Verify(context.Background(), known); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	notify := sync.OnceFunc(func() { close(started) })
	f.beforeKeys.Store(func() {
		notify()
		<-release
	})
	refresher := make(chan error, 1)
	go func() {
		_, err := v.Verify(context.Background(), unknown)
		refresher <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("key set refresh did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	waiter := make(chan error, 1)
	go func() {
		_, err := v.Verify(ctx, unknown)
		waiter <- err
	}()
	select {
	case err := <-waiter:
		if err != ErrOIDCUnavailable {
			t.Errorf("canceled waiter: %v, want unavailable", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("canceled waiter kept waiting for the refresh")
	}
	unblock()
	if err := <-refresher; err != ErrOIDCInvalid {
		t.Errorf("refreshing caller: %v", err)
	}
}

func TestOIDCCachedKeyDuringRefresh(t *testing.T) {
	f := newOIDCFixture(t)
	v := f.verifier(t)
	known := f.sign(t, oidcHeader("RS256", "rsa"), f.claims(), "RS256")
	unknown := f.sign(t, oidcHeader("RS256", "unknown"), f.claims(), "RS256")
	verify := func(raw string) error {
		_, err := v.Verify(context.Background(), raw)
		return err
	}
	if err := verify(known); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	notify := sync.OnceFunc(func() { close(started) })
	f.beforeKeys.Store(func() {
		notify()
		<-release
	})
	misses := make(chan error, 2)
	go func() { misses <- verify(unknown) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("key set refresh did not start")
	}
	go func() { misses <- verify(unknown) }()
	cached := make(chan error, 1)
	go func() { cached <- verify(known) }()
	select {
	case err := <-cached:
		if err != nil {
			t.Errorf("cached key: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("cached key waited for the refresh")
	}
	unblock()
	for range 2 {
		if err := <-misses; err != ErrOIDCInvalid {
			t.Errorf("unknown key: %v", err)
		}
	}
	if got := f.fetches.Load() - 1; got != 1 {
		t.Fatalf("key set refresh requests = %d, want one", got)
	}
}

func TestOIDCRediscoversAfterKeySetFailure(t *testing.T) {
	for _, failure := range []string{"redirect", "invalid JSON"} {
		t.Run(failure, func(t *testing.T) {
			f := newOIDCFixture(t)
			v := f.verifier(t)
			body := oidcJSON(t, map[string]any{"keys": []map[string]any{oidcRSAKey("rotated", &f.rsa.PublicKey)}})
			var replacementFetches atomic.Int32
			replacement := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				replacementFetches.Add(1)
				_, _ = w.Write(body)
			}))
			defer replacement.Close()
			verify := func(kid string) error {
				raw := f.sign(t, oidcHeader("RS256", kid), f.claims(), "RS256")
				_, err := v.Verify(context.Background(), raw)
				return err
			}
			if err := verify("rsa"); err != nil {
				t.Fatal(err)
			}
			f.keys.Store(failure)
			f.discovery.Store(string(oidcJSON(t, map[string]string{"issuer": f.issuer, "jwks_uri": replacement.URL})))
			if err := verify("unknown"); err != ErrOIDCUnavailable {
				t.Fatalf("failed key set request: %v", err)
			}
			f.now = f.now.Add(59 * time.Second)
			if err := verify("unknown"); err != ErrOIDCInvalid || f.documents.Load() != 1 || f.fetches.Load() != 2 {
				t.Fatalf("before cooldown: %v, discovery = %d, keys = %d", err, f.documents.Load(), f.fetches.Load())
			}
			f.now = f.now.Add(time.Second)
			if err := verify("rotated"); err != nil || f.documents.Load() != 2 ||
				f.fetches.Load() != 2 || replacementFetches.Load() != 1 {
				t.Fatalf("after cooldown: %v, discovery = %d, old keys = %d, new keys = %d",
					err, f.documents.Load(), f.fetches.Load(), replacementFetches.Load())
			}
		})
	}
}

func TestOIDCDiscoveryFailuresAndRetry(t *testing.T) {
	f := newOIDCFixture(t)
	original := f.discovery.Load().(string)
	for _, tc := range []struct{ name, document string }{
		{"issuer mismatch", strings.Replace(original, f.issuer, f.issuer+"other", 1)},
		{"non-loopback JWKS", string(oidcJSON(t, map[string]string{"issuer": f.issuer, "jwks_uri": "http://example.invalid/keys"}))},
		{"JWKS user info", string(oidcJSON(t, map[string]string{"issuer": f.issuer, "jwks_uri": "http://user@127.0.0.1/keys"}))},
		{"redirect", "redirect"},
		{"oversize body", original + strings.Repeat(" ", 1<<20)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beforeDocs, beforeKeys := f.documents.Load(), f.fetches.Load()
			f.discovery.Store(tc.document)
			v := f.verifier(t)
			raw := f.sign(t, oidcHeader("RS256", "rsa"), f.claims(), "RS256")
			if _, err := v.Verify(context.Background(), raw); err != ErrOIDCUnavailable {
				t.Fatalf("Verify = %v", err)
			}
			f.discovery.Store(original)
			if _, err := v.Verify(context.Background(), raw); err != ErrOIDCUnavailable ||
				f.documents.Load() != beforeDocs+1 || f.fetches.Load() != beforeKeys {
				t.Fatalf("immediate retry: %v, discovery = %d, keys = %d", err, f.documents.Load(), f.fetches.Load())
			}
			f.now = f.now.Add(61 * time.Second)
			if _, err := v.Verify(context.Background(), raw); err != nil || f.fetches.Load() != beforeKeys+1 {
				t.Fatalf("retry after cooldown: %v, keys = %d", err, f.fetches.Load())
			}
		})
	}
}

func TestOIDCJWKSBounds(t *testing.T) {
	f := newOIDCFixture(t)
	raw := f.sign(t, oidcHeader("RS256", "rsa"), f.claims(), "RS256")
	key := oidcRSAKey("rsa", &f.rsa.PublicKey)
	entries := make([]map[string]any, 101)
	entries[100] = key
	f.setKeys(t, entries...)
	largeSet := f.keys.Load().(string)
	f.setKeys(t, key, key)
	duplicate := f.keys.Load().(string)
	initial := f.documents.Load()
	for _, tc := range []struct {
		name, body string
		want       error
	}{
		{"over 100 keys", largeSet, ErrOIDCInvalid},
		{"duplicate kid", duplicate, ErrOIDCInvalid},
		{"oversize body", largeSet + strings.Repeat(" ", 1<<20), ErrOIDCUnavailable},
		{"redirect", "redirect", ErrOIDCUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.keys.Store(tc.body)
			if _, err := f.verifier(t).Verify(context.Background(), raw); err != tc.want {
				t.Fatalf("Verify = %v, want %v", err, tc.want)
			}
		})
	}
	if f.documents.Load() != initial+4 || f.fetches.Load() != 4 {
		t.Fatalf("discovery requests = %d and key requests = %d, want four each", f.documents.Load(), f.fetches.Load())
	}
}
