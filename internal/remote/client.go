package remote

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

var ErrMalformedEvent = errors.New("malformed event from control service")
var ErrEventOutput = errors.New("failed to write event output")

// Client uses tenant credentials only. It never sends a token to a redirect.
type Client struct {
	origin string
	token  string
	http   *http.Client
}

func NewFromEnv() (*Client, error)         { return newFromCredentialEnv("HRN_TENANT_TOKEN") }
func NewOperatorFromEnv() (*Client, error) { return newFromCredentialEnv("HRN_OPERATOR_TOKEN") }
func newFromCredentialEnv(credentialEnv string) (*Client, error) {
	raw := os.Getenv("HRN_SERVER_URL")
	token := os.Getenv(credentialEnv)
	if raw == "" || len(token) < 32 {
		return nil, fmt.Errorf("HRN_SERVER_URL and %s (at least 32 characters) are required", credentialEnv)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" ||
		parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("HRN_SERVER_URL must be an origin without credentials, path or query")
	}
	host := parsed.Hostname()
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" &&
		net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()) {
		return nil, errors.New("remote control requires HTTPS or loopback HTTP")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if caFile := os.Getenv("HRN_SERVER_CA_FILE"); caFile != "" {
		if parsed.Scheme != "https" {
			return nil, errors.New("HRN_SERVER_CA_FILE requires HTTPS")
		}
		info, err := os.Stat(caFile)
		if err != nil || info.Size() > 1<<20 {
			return nil, errors.New("invalid remote CA file")
		}
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("remote CA file contains no certificates")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &Client{origin: strings.TrimRight(raw, "/"), token: token,
		http: &http.Client{Transport: transport, Timeout: 20 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *Client) Request(ctx context.Context, method, path string, input any) ([]byte, error) {
	var body io.Reader
	if input != nil {
		payload, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.origin+path, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 4<<20 {
		return nil, errors.New("control response exceeds 4 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &HTTPError{Status: response.StatusCode}
	}
	return data, nil
}

func (c *Client) Watch(ctx context.Context, path string, after int64, out io.Writer) error {
	watchClient := *c.http
	watchClient.Timeout = 0
	failures := 0
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.origin+path, nil)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+c.token)
		request.Header.Set("Accept", "text/event-stream")
		request.Header.Set("Last-Event-ID", strconv.FormatInt(after, 10))
		response, err := watchClient.Do(request)
		if err == nil {
			if response.StatusCode != http.StatusOK {
				response.Body.Close()
				err = &HTTPError{Status: response.StatusCode}
				if response.StatusCode < 500 {
					return err
				}
			} else {
				failures = 0
				after, err = ReadEvents(response.Body, after, out)
				response.Body.Close()
				if ctx.Err() != nil {
					return nil
				}
				if errors.Is(err, ErrMalformedEvent) || errors.Is(err, ErrEventOutput) {
					return err
				}
			}
		} else {
			var unknown x509.UnknownAuthorityError
			var hostname x509.HostnameError
			var invalid x509.CertificateInvalidError
			if errors.As(err, &unknown) || errors.As(err, &hostname) || errors.As(err, &invalid) {
				return err
			}
		}
		if err != nil {
			failures++
			if failures >= 30 {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
}

func ReadEvents(input io.Reader, after int64, out io.Writer) (int64, error) {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var currentID int64
	var data string
	emit := func() error {
		if data == "" {
			return nil
		}
		if currentID <= 0 {
			return fmt.Errorf("%w: missing positive ID", ErrMalformedEvent)
		}
		if currentID <= after {
			return nil
		}
		if !json.Valid([]byte(data)) {
			return fmt.Errorf("%w: invalid JSON", ErrMalformedEvent)
		}
		if _, err := fmt.Fprintln(out, data); err != nil {
			return fmt.Errorf("%w: %w", ErrEventOutput, err)
		}
		after = currentID
		return nil
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := emit(); err != nil {
				return after, err
			}
			currentID, data = 0, ""
			continue
		}
		if value, ok := strings.CutPrefix(line, "id: "); ok {
			id, err := strconv.ParseInt(value, 10, 64)
			if err != nil || id < 0 {
				return after, fmt.Errorf("%w: invalid ID", ErrMalformedEvent)
			}
			currentID = id
		} else if value, ok := strings.CutPrefix(line, "data: "); ok {
			if data != "" {
				data += "\n"
			}
			data += value
		}
	}
	if err := scanner.Err(); err != nil {
		return after, err
	}
	if data != "" {
		if err := emit(); err != nil {
			return after, err
		}
	}
	return after, nil
}

// HTTPError omits untrusted error bodies, which could echo credentials.
type HTTPError struct{ Status int }

func (e *HTTPError) Error() string { return fmt.Sprintf("control service HTTP %d", e.Status) }
