package remote

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/cristiangirlea/harness/internal/retrieval"
)

func TestRetrievalClientValidatesPacket(t *testing.T) {
	var mu sync.Mutex
	text := fmt.Sprintf("[Reference %s; source=%q; revision=%s; evidence=%q]\n%s\n[/Reference]\n", "e_fixture", "", "main", "", "reference")
	packet := retrieval.Packet{Text: text, Bytes: len(text), Digest: retrieval.TextDigest(text), VersionID: "v_pin", QueryDigest: retrieval.TextDigest(""), Entries: []retrieval.Entry{{ID: "e_fixture", Content: "reference"}}}
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
	packet.Bytes = len(text) - 1
	mu.Unlock()
	if _, e = c.SearchKnowledge(context.Background(), in); e == nil {
		t.Fatal("bad byte count accepted")
	}
	mu.Lock()
	packet.Bytes = len(text)
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

func TestRetrievalClientClassifiesKnowledgeAuthorizationStorageFailureAsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"retrieval_busy","message":"retrieval capacity unavailable"}`))
	}))
	defer srv.Close()
	t.Setenv("HRN_SERVER_URL", srv.URL)
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("t", 64))
	t.Setenv("HRN_SERVER_CA_FILE", "")
	c, err := NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.SearchKnowledge(context.Background(), retrieval.Search{})
	if err == nil || !RetrievalUnavailable(err) {
		t.Fatalf("authorization storage failure classified as available or missing: %v", err)
	}
}

func TestRetrievalTLSProtocolFailuresDoNotCountAsUnavailable(t *testing.T) {
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

func TestRetrievalClientRejectsCredentialEchoAndQueryDigestMismatch(t *testing.T) {
	token := strings.Repeat("t", 64)
	query := "fixture query"
	packet := retrieval.Packet{Entries: []retrieval.Entry{}, QueryDigest: retrieval.TextDigest(query), Digest: retrieval.TextDigest("")}
	var response string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(response)) }))
	defer srv.Close()
	t.Setenv("HRN_SERVER_URL", srv.URL)
	t.Setenv("HRN_TENANT_TOKEN", token)
	t.Setenv("HRN_SERVER_CA_FILE", "")
	c, err := NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		packet retrieval.Packet
		echo   bool
	}{
		{"credential echo", packet, true},
		{"query digest", func() retrieval.Packet { p := packet; p.QueryDigest = retrieval.TextDigest("different"); return p }(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.packet)
			if err != nil {
				t.Fatal(err)
			}
			response = string(b)
			if tc.echo {
				response = strings.TrimSuffix(response, "}") + `,"echo":"` + token + `"}`
			}
			_, err = c.SearchKnowledge(context.Background(), retrieval.Search{Query: query})
			want := "query digest"
			if tc.echo {
				want = "reflected a credential"
			}
			if err == nil || !strings.Contains(err.Error(), want) || RetrievalUnavailable(err) {
				t.Fatalf("response not refused for %s: %v", want, err)
			}
		})
	}
}
