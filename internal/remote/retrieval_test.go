package remote

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/cristiangirlea/harness/internal/retrieval"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestRetrievalClientValidatesPacket(t *testing.T) {
	var mu sync.Mutex
	hash := sha256.Sum256([]byte("reference"))
	packet := retrieval.Packet{Text: "reference", Bytes: 9, Digest: hex.EncodeToString(hash[:]), VersionID: "v_pin", QueryDigest: retrieval.TextDigest(""), Entries: []retrieval.Entry{{ID: "e_fixture"}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/knowledge/search" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("t", 64) {
			t.Error("request identity")
		}
		mu.Lock()
		defer mu.Unlock()
		json.NewEncoder(w).Encode(packet)
	}))
	defer srv.Close()
	t.Setenv("HRN_SERVER_URL", srv.URL)
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("t", 64))
	t.Setenv("HRN_SERVER_CA_FILE", "")
	c, e := NewFromEnv()
	if e != nil {
		t.Fatal(e)
	}
	in := retrieval.Search{VersionID: "v_pin"}
	if _, e = c.SearchKnowledge(context.Background(), in); e != nil {
		t.Fatal(e)
	}
	mu.Lock()
	packet.Bytes = 8
	mu.Unlock()
	if _, e = c.SearchKnowledge(context.Background(), in); e == nil {
		t.Fatal("bad byte count accepted")
	}
	mu.Lock()
	packet.Bytes = 9
	mu.Unlock()
	mu.Lock()
	packet.VersionID = "v_other"
	mu.Unlock()
	if _, e = c.SearchKnowledge(context.Background(), in); e == nil {
		t.Fatal("different pin accepted")
	}
	mu.Lock()
	packet.VersionID = "v_pin"
	mu.Unlock()
	mu.Lock()
	packet.Digest = "wrong"
	mu.Unlock()
	if _, e = c.SearchKnowledge(context.Background(), in); e == nil {
		t.Fatal("bad digest accepted")
	}
}

func TestRetrievalTLSWrappedProtocolFailures(t *testing.T) {
	for _, cause := range []error{
		x509.CertificateInvalidError{Cert: &x509.Certificate{}, Reason: x509.Expired},
		tls.RecordHeaderError{Msg: "malformed handshake"},
		errors.New("tls: server sent an unexpected message"),
	} {
		for _, err := range []error{cause, &url.Error{Op: "Post", URL: "https://fixture.invalid", Err: cause}} {
			if RetrievalUnavailable(err) {
				t.Fatalf("security/protocol failure treated as availability: %T", cause)
			}
		}
	}
	// Exercise the actual HTTP transport's plaintext response to a TLS handshake.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	t.Setenv("HRN_SERVER_URL", strings.Replace(srv.URL, "http:", "https:", 1))
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("t", 64))
	t.Setenv("HRN_SERVER_CA_FILE", "")
	c, err := NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.SearchKnowledge(context.Background(), retrieval.Search{})
	if err == nil || RetrievalUnavailable(err) {
		t.Fatalf("malformed live handshake skipped: %v", err)
	}
}

func TestRetrievalRedirectNeverForwardsCredential(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("redirect followed") }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	t.Setenv("HRN_SERVER_URL", srv.URL)
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("t", 64))
	t.Setenv("HRN_SERVER_CA_FILE", "")
	c, e := NewFromEnv()
	if e != nil {
		t.Fatal(e)
	}
	_, e = c.SearchKnowledge(context.Background(), retrieval.Search{})
	if e == nil || RetrievalUnavailable(e) {
		t.Fatal("redirect treated as optional availability")
	}
}

func TestRetrievalTLSFailureIsNotOptionalAvailability(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted TLS request reached service") }))
	defer srv.Close()
	t.Setenv("HRN_SERVER_URL", srv.URL)
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("t", 64))
	t.Setenv("HRN_SERVER_CA_FILE", "")
	c, e := NewFromEnv()
	if e != nil {
		t.Fatal(e)
	}
	_, e = c.SearchKnowledge(context.Background(), retrieval.Search{})
	if e == nil || RetrievalUnavailable(e) {
		t.Fatal("TLS verification bypassed optional policy")
	}
}
