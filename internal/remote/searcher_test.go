package remote

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cristiangirlea/harness/internal/retrieval"
)

type searchTransport func(*http.Request) (*http.Response, error)

func (f searchTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSearcherClassifiesFailuresAndPreservesTheirCause(t *testing.T) {
	cases := []struct {
		name        string
		cause       error
		unavailable bool
	}{
		{"server", &HTTPError{Status: 503}, true},
		{"rate limit", &HTTPError{Status: 429}, true},
		{"client", &HTTPError{Status: 403}, false},
		{"network", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}, true},
		{"deadline", context.DeadlineExceeded, true},
		{"canceled", context.Canceled, false},
		{"authority", x509.UnknownAuthorityError{}, false},
		{"hostname", x509.HostnameError{Certificate: &x509.Certificate{}}, false},
		{"certificate", x509.CertificateInvalidError{Cert: &x509.Certificate{}, Reason: x509.Expired}, false},
		{"verification", &tls.CertificateVerificationError{Err: errors.New("bad certificate")}, false},
		{"record header", tls.RecordHeaderError{Msg: "bad record"}, false},
		{"other", errors.New("other"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{origin: "http://127.0.0.1", token: "fixture", http: &http.Client{Transport: searchTransport(func(*http.Request) (*http.Response, error) {
				return nil, tc.cause
			})}}
			_, err := Searcher(c).SearchKnowledge(context.Background(), retrieval.Search{})
			if !errors.Is(err, tc.cause) || errors.Is(err, retrieval.ErrUnavailable) != tc.unavailable {
				t.Fatalf("cause or classification lost: %v", err)
			}
		})
	}
}

func TestSearcherClassifiesHTTPResponses(t *testing.T) {
	for _, tc := range []struct {
		status      int
		unavailable bool
	}{{500, true}, {503, true}, {429, true}, {400, false}, {401, false}, {403, false}, {404, false}} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status) }))
			defer srv.Close()
			c := &Client{origin: srv.URL, token: "fixture", http: srv.Client()}
			_, err := Searcher(c).SearchKnowledge(context.Background(), retrieval.Search{})
			var cause *HTTPError
			if !errors.As(err, &cause) || cause.Status != tc.status || errors.Is(err, retrieval.ErrUnavailable) != tc.unavailable {
				t.Fatalf("status %d: %v", tc.status, err)
			}
		})
	}
}
