package platform

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	ErrOIDCInvalid     = errors.New("invalid OpenID Connect token")
	ErrOIDCUnavailable = errors.New("OpenID Connect keys unavailable")
)

type OIDCConfig struct {
	Issuer, Audience             string
	OperatorClaim, OperatorValue string
	TenantClaim                  string
}

type Identity struct {
	Subject, Issuer string
	Operator        bool
	Tenants         []string
}

type OIDCVerifier struct {
	config OIDCConfig
	client *http.Client
	now    func() time.Time

	mu         sync.Mutex
	jwksURI    string
	keys       map[string]oidcKey
	retryAfter time.Time
	refreshing chan struct{}
}

func NewOIDCVerifier(config OIDCConfig) (*OIDCVerifier, error) {
	switch {
	case config == (OIDCConfig{}):
		return nil, nil
	case config.Issuer == "" && config.Audience != "":
		return nil, errors.New("HRN_OIDC_AUDIENCE requires HRN_OIDC_ISSUER")
	case config.Issuer == "" && config.OperatorClaim != "":
		return nil, errors.New("HRN_OIDC_OPERATOR_CLAIM requires HRN_OIDC_ISSUER")
	case config.Issuer == "" && config.OperatorValue != "":
		return nil, errors.New("HRN_OIDC_OPERATOR_VALUE requires HRN_OIDC_ISSUER")
	case config.Issuer == "" && config.TenantClaim != "":
		return nil, errors.New("HRN_OIDC_TENANT_CLAIM requires HRN_OIDC_ISSUER")
	}
	issuer := oidcURL(config.Issuer)
	switch {
	case issuer == nil || issuer.ForceQuery || issuer.RawQuery != "" || strings.Contains(config.Issuer, "#"):
		return nil, errors.New("invalid HRN_OIDC_ISSUER")
	case config.Audience == "":
		return nil, errors.New("HRN_OIDC_AUDIENCE is required")
	case config.OperatorClaim == "" && config.OperatorValue != "":
		return nil, errors.New("HRN_OIDC_OPERATOR_CLAIM is required with HRN_OIDC_OPERATOR_VALUE")
	case config.OperatorClaim != "" && config.OperatorValue == "":
		return nil, errors.New("HRN_OIDC_OPERATOR_VALUE is required with HRN_OIDC_OPERATOR_CLAIM")
	case config.OperatorClaim != "" && !validOIDCClaimName(config.OperatorClaim):
		return nil, errors.New("invalid HRN_OIDC_OPERATOR_CLAIM")
	case config.TenantClaim != "" && !validOIDCClaimName(config.TenantClaim):
		return nil, errors.New("invalid HRN_OIDC_TENANT_CLAIM")
	case config.OperatorClaim == "" && config.TenantClaim == "":
		return nil, errors.New("HRN_OIDC_OPERATOR_CLAIM or HRN_OIDC_TENANT_CLAIM is required")
	}
	return &OIDCVerifier{
		config: config,
		now:    time.Now,
		client: &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func validOIDCClaimName(name string) bool {
	return oidcText(name, 128) && !strings.ContainsAny(name, "./\\[]$") &&
		strings.IndexFunc(name, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) == -1
}

func oidcText(s string, max int) bool {
	return s != "" && utf8.ValidString(s) && utf8.RuneCountInString(s) <= max
}

func oidcURL(raw string) *url.URL {
	if strings.Contains(raw, "\\") || strings.IndexFunc(raw, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) != -1 {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return nil
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
			return nil
		}
	}
	switch u.Scheme {
	case "https":
		return u
	case "http":
		addr, err := netip.ParseAddr(u.Hostname())
		if err == nil && addr.Zone() == "" && !addr.Is4In6() && addr.IsLoopback() {
			return u
		}
	}
	return nil
}

var oidcEncoding = base64.RawURLEncoding.Strict()

func oidcSegment(raw string) []byte {
	if raw == "" || strings.ContainsAny(raw, "\r\n") {
		return nil
	}
	decoded, err := oidcEncoding.DecodeString(raw)
	if err != nil {
		return nil
	}
	return decoded
}

func oidcObject[T any](raw []byte) map[string]T {
	if !utf8.Valid(raw) {
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return nil
	}
	obj := make(map[string]T)
	for d.More() {
		token, err := d.Token()
		name, ok := token.(string)
		if err != nil || !ok {
			return nil
		}
		var value T
		if _, duplicate := obj[name]; duplicate || d.Decode(&value) != nil {
			return nil
		}
		obj[name] = value
	}
	if end, err := d.Token(); err != nil || end != json.Delim('}') {
		return nil
	}
	if _, err := d.Token(); err != io.EOF {
		return nil
	}
	return obj
}

func (v *OIDCVerifier) Verify(ctx context.Context, raw string) (Identity, error) {
	if len(raw) > 8192 || strings.Count(raw, ".") != 2 {
		return Identity{}, ErrOIDCInvalid
	}
	header, rest, _ := strings.Cut(raw, ".")
	payload, signature, _ := strings.Cut(rest, ".")
	fields := oidcObject[any](oidcSegment(header))
	alg, _ := fields["alg"].(string)
	kid, _ := fields["kid"].(string)
	if (alg != "RS256" && alg != "ES256") || !oidcText(kid, 256) {
		return Identity{}, ErrOIDCInvalid
	}
	for _, forbidden := range []string{"crit", "jku", "jwk", "x5u", "x5c"} {
		if _, present := fields[forbidden]; present {
			return Identity{}, ErrOIDCInvalid
		}
	}
	claims := oidcObject[any](oidcSegment(payload))
	sig := oidcSegment(signature)
	if claims == nil || len(sig) == 0 || (alg == "ES256" && len(sig) != 64) {
		return Identity{}, ErrOIDCInvalid
	}
	key, err := v.key(ctx, kid)
	if err != nil {
		return Identity{}, err
	}
	digest := sha256.Sum256([]byte(raw[:len(header)+1+len(payload)]))
	if !key.verify(alg, digest[:], sig) {
		return Identity{}, ErrOIDCInvalid
	}
	return v.identity(claims)
}

func (v *OIDCVerifier) identity(claims map[string]any) (Identity, error) {
	issuer, _ := claims["iss"].(string)
	subject, _ := claims["sub"].(string)
	now := v.now()
	seconds := float64(now.Unix()) + float64(now.Nanosecond())/1e9
	exp, valid := oidcDate(claims["exp"])
	if issuer != v.config.Issuer || !oidcContains(claims["aud"], v.config.Audience) ||
		!oidcText(subject, 256) || strings.IndexFunc(subject, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 ||
		!valid || exp <= seconds-60 {
		return Identity{}, ErrOIDCInvalid
	}
	for _, name := range []string{"nbf", "iat"} {
		if value, present := claims[name]; present {
			when, valid := oidcDate(value)
			if !valid || when > seconds+60 {
				return Identity{}, ErrOIDCInvalid
			}
		}
	}
	id := Identity{Subject: subject, Issuer: issuer}
	if v.config.OperatorClaim != "" {
		id.Operator = oidcContains(claims[v.config.OperatorClaim], v.config.OperatorValue)
	}
	if v.config.TenantClaim != "" {
		switch value := claims[v.config.TenantClaim].(type) {
		case string:
			if value != "" {
				id.Tenants = []string{value}
			}
		case []any:
			for _, item := range value {
				if tenant, ok := item.(string); ok && tenant != "" {
					id.Tenants = append(id.Tenants, tenant)
					if len(id.Tenants) == 256 {
						break
					}
				}
			}
		}
	}
	return id, nil
}

func oidcContains(value any, wanted string) bool {
	switch value := value.(type) {
	case string:
		return value == wanted
	case []any:
		found := false
		for _, item := range value {
			s, ok := item.(string)
			if !ok {
				return false
			}
			found = found || s == wanted
		}
		return found
	}
	return false
}

func oidcDate(value any) (float64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	seconds, err := number.Float64()
	return seconds, err == nil && math.Abs(seconds) <= 1<<53
}

type oidcKey struct {
	rsa *rsa.PublicKey
	ec  *ecdsa.PublicKey
}

func (k oidcKey) verify(alg string, digest, sig []byte) bool {
	switch alg {
	case "RS256":
		return k.rsa != nil && rsa.VerifyPKCS1v15(k.rsa, crypto.SHA256, digest, sig) == nil
	case "ES256":
		return k.ec != nil && ecdsa.Verify(k.ec, digest, new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
	}
	return false
}

func parseOIDCKey(fields map[string]any) oidcKey {
	if use, present := fields["use"]; present && use != "sig" {
		return oidcKey{}
	}
	decode := func(name string) []byte {
		text, _ := fields[name].(string)
		return oidcSegment(text)
	}
	alg, declared := fields["alg"]
	switch fields["kty"] {
	case "RSA":
		if declared && alg != "RS256" {
			return oidcKey{}
		}
		n := new(big.Int).SetBytes(decode("n"))
		e := new(big.Int).SetBytes(decode("e"))
		if n.BitLen() >= 2048 && n.Bit(0) == 1 && e.BitLen() <= 31 && e.Int64() >= 3 && e.Bit(0) == 1 {
			return oidcKey{rsa: &rsa.PublicKey{N: n, E: int(e.Int64())}}
		}
	case "EC":
		if fields["crv"] != "P-256" || (declared && alg != "ES256") {
			return oidcKey{}
		}
		xb, yb := decode("x"), decode("y")
		x, y := new(big.Int).SetBytes(xb), new(big.Int).SetBytes(yb)
		curve := elliptic.P256()
		if len(xb) == 32 && len(yb) == 32 && curve.IsOnCurve(x, y) {
			return oidcKey{ec: &ecdsa.PublicKey{Curve: curve, X: x, Y: y}}
		}
	}
	return oidcKey{}
}

func (v *OIDCVerifier) key(ctx context.Context, kid string) (oidcKey, error) {
	v.mu.Lock()
	if key, present := v.keys[kid]; present {
		v.mu.Unlock()
		return key, nil
	}
	if done := v.refreshing; done != nil {
		v.mu.Unlock()
		select {
		case <-done:
		// A caller that hung up stops waiting; the shared refresh goes on.
		case <-ctx.Done():
			return oidcKey{}, ErrOIDCUnavailable
		}
		v.mu.Lock()
	} else if !v.now().Before(v.retryAfter) {
		done := make(chan struct{})
		v.refreshing = done
		first, uri := v.keys == nil, v.jwksURI
		v.mu.Unlock()

		uri, keys, err := v.refresh(context.WithoutCancel(ctx), uri)

		v.mu.Lock()
		v.jwksURI = uri
		if err == nil {
			v.keys = keys
		}
		// A successful initial load for a known key leaves the first rotation refresh available.
		if _, present := keys[kid]; !first || !present {
			v.retryAfter = v.now().Add(time.Minute)
		}
		v.refreshing = nil
		close(done)
		if err != nil {
			v.mu.Unlock()
			return oidcKey{}, err
		}
	}
	key, present := v.keys[kid]
	loaded := v.keys != nil
	v.mu.Unlock()
	if !loaded {
		return oidcKey{}, ErrOIDCUnavailable
	}
	if !present {
		return oidcKey{}, ErrOIDCInvalid
	}
	return key, nil
}

func (v *OIDCVerifier) refresh(ctx context.Context, uri string) (string, map[string]oidcKey, error) {
	if uri == "" {
		raw, err := v.get(ctx, strings.TrimRight(v.config.Issuer, "/")+"/.well-known/openid-configuration")
		if err != nil {
			return "", nil, err
		}
		doc := oidcObject[any](raw)
		issuer, _ := doc["issuer"].(string)
		uri, _ = doc["jwks_uri"].(string)
		if issuer != v.config.Issuer || oidcURL(uri) == nil {
			return "", nil, ErrOIDCUnavailable
		}
	}
	raw, err := v.get(ctx, uri)
	if err != nil {
		return "", nil, err
	}
	doc := oidcObject[json.RawMessage](raw)
	var entries []json.RawMessage
	if json.Unmarshal(doc["keys"], &entries) != nil || entries == nil {
		return "", nil, ErrOIDCUnavailable
	}
	keys := make(map[string]oidcKey)
	for _, entry := range entries[:min(len(entries), 100)] {
		fields := oidcObject[any](entry)
		kid, _ := fields["kid"].(string)
		if !oidcText(kid, 256) {
			continue
		}
		if _, duplicate := keys[kid]; duplicate {
			keys[kid] = oidcKey{}
		} else {
			keys[kid] = parseOIDCKey(fields)
		}
	}
	return uri, keys, nil
}

func (v *OIDCVerifier) get(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, ErrOIDCUnavailable
	}
	response, err := v.client.Do(req)
	if err != nil {
		return nil, ErrOIDCUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, ErrOIDCUnavailable
	}
	const maxBody = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil || len(raw) > maxBody {
		return nil, ErrOIDCUnavailable
	}
	return raw, nil
}
