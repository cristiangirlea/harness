package remote

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cristiangirlea/harness/internal/retrieval"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Optional retrieval skips only availability failures (transport, 5xx/429); all other failures stay closed.
func RetrievalUnavailable(err error) bool {
	var h *HTTPError
	if errors.As(err, &h) {
		return h.Status >= 500 || h.Status == 429
	}
	var verification *tls.CertificateVerificationError
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var header tls.RecordHeaderError
	if errors.As(err, &verification) || errors.As(err, &unknown) || errors.As(err, &hostname) || errors.As(err, &invalid) || errors.As(err, &header) {
		return false
	}
	// Inspect url.Error's cause so its net.Error interface cannot make malformed TLS skippable.
	var u *url.Error
	if errors.As(err, &u) {
		return RetrievalUnavailable(u.Err)
	}
	var n net.Error
	return errors.As(err, &n) || errors.Is(err, context.DeadlineExceeded)
}
func (c *Client) SearchKnowledge(ctx context.Context, in retrieval.Search) (retrieval.Packet, error) {
	var out retrieval.Packet
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	b, err := c.Request(ctx, http.MethodPost, "/v1/knowledge/search", in)
	if err != nil {
		return out, err
	}
	if strings.Contains(string(b), c.token) {
		return out, fmt.Errorf("retrieval response reflected a credential")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&out); err != nil {
		return out, fmt.Errorf("malformed retrieval packet")
	}
	if d.Decode(new(any)) != io.EOF || out.QueryDigest != retrieval.TextDigest(in.Query) {
		return retrieval.Packet{}, fmt.Errorf("malformed retrieval packet or query digest")
	}
	if err = retrieval.ValidatePacket(out, in.VersionID, in.Query); err != nil {
		return retrieval.Packet{}, err
	}
	return out, nil
}

func (c *Client) RecordAdmission(ctx context.Context, in retrieval.Admission) error {
	_, err := c.Request(ctx, http.MethodPost, "/v1/knowledge/admission", in)
	return err
}
