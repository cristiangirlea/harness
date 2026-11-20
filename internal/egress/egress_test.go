package egress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// publicAddr stands in for what a real resolver would answer; nothing dials it.
var publicAddr = netip.MustParseAddr("93.184.216.34")

type denial struct{ target, reason string }

type harness struct {
	proxy  *Proxy
	dials  atomic.Int32
	mu     sync.Mutex
	denied []denial
}

func (h *harness) denials() []denial {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]denial(nil), h.denied...)
}

// start runs a proxy whose resolver answers answers for every name and whose
// dialer reaches origin when it is asked for the address it checked.
func start(t *testing.T, hosts []string, answers []netip.Addr, origin string, tune func(*Options)) *harness {
	t.Helper()
	h := &harness{}
	o := Options{
		Hosts: hosts,
		Resolve: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return answers, nil
		},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			h.dials.Add(1)
			if addr != net.JoinHostPort(answers[0].String(), "443") {
				return nil, fmt.Errorf("dialled %s, not the checked address", addr)
			}
			var d net.Dialer
			return d.DialContext(ctx, network, origin)
		},
		OnDeny: func(target, reason string) {
			h.mu.Lock()
			h.denied = append(h.denied, denial{target, reason})
			h.mu.Unlock()
		},
	}
	if tune != nil {
		tune(&o)
	}
	p, err := Start(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	h.proxy = p
	return h
}

// send writes raw request bytes to the proxy and returns the status code it answers.
func send(t *testing.T, p *Proxy, raw string) (int, net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, raw); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("reading the proxy's answer: %v", err)
	}
	c.SetDeadline(time.Time{})
	return resp.StatusCode, c, br
}

func TestAnAllowedHostIsTunnelledToTheOrigin(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "origin reached")
	}))
	defer origin.Close()
	// httptest's certificate names example.com, so TLS verifies end to end.
	h := start(t, []string{"example.com"}, []netip.Addr{publicAddr}, origin.Listener.Addr().String(), nil)
	if !strings.HasPrefix(h.proxy.URL(), "http://127.0.0.1:") {
		t.Fatalf("proxy URL %s is not on loopback", h.proxy.URL())
	}
	tr := origin.Client().Transport.(*http.Transport).Clone()
	proxyURL, _ := url.Parse(h.proxy.URL())
	tr.Proxy = http.ProxyURL(proxyURL)
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Get("https://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "origin reached" {
		t.Fatalf("body %q", body)
	}
	if d := h.denials(); len(d) != 0 {
		t.Errorf("an allowed host was refused: %v", d)
	}
}

func TestRefusedRequestsNeverDial(t *testing.T) {
	for _, tc := range []struct{ name, raw, target, reason string }{
		{"host not on the list", "CONNECT other.example.org:443 HTTP/1.1\r\nHost: other.example.org:443\r\n\r\n", "other.example.org:443", "not allowed"},
		{"port other than 443", "CONNECT example.com:80 HTTP/1.1\r\nHost: example.com:80\r\n\r\n", "example.com:80", "port not 443"},
		{"plain HTTP", "GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n", "example.com", "not CONNECT"},
		{"IPv4 literal", "CONNECT 93.184.216.34:443 HTTP/1.1\r\nHost: 93.184.216.34:443\r\n\r\n", "93.184.216.34:443", "IP literal"},
		{"IPv6 literal", "CONNECT [2606:4700::1]:443 HTTP/1.1\r\nHost: [2606:4700::1]:443\r\n\r\n", "[2606:4700::1]:443", "IP literal"},
		{"non-ASCII name", "CONNECT b\xc3\xa9.example:443 HTTP/1.1\r\n\r\n", "b?.example:443", "not allowed"},
		{"trailing dot", "CONNECT example.com.:443 HTTP/1.1\r\nHost: example.com.:443\r\n\r\n", "example.com.:443", "not allowed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := start(t, []string{"example.com"}, []netip.Addr{publicAddr}, "127.0.0.1:1", nil)
			code, _, _ := send(t, h.proxy, tc.raw)
			if code != http.StatusForbidden {
				t.Errorf("status %d, want 403", code)
			}
			if n := h.dials.Load(); n != 0 {
				t.Errorf("the proxy dialled %d times", n)
			}
			if d := h.denials(); len(d) != 1 || d[0] != (denial{tc.target, tc.reason}) {
				t.Errorf("denials %v, want %s (%s)", d, tc.target, tc.reason)
			}
		})
	}
}

func TestAMalformedRequestIsAnsweredAndRecorded(t *testing.T) {
	h := start(t, []string{"example.com"}, []netip.Addr{publicAddr}, "127.0.0.1:1", nil)
	code, _, _ := send(t, h.proxy, "CONNECT bad\x1b[31m.example:443 HTTP/1.1\r\n\r\n")
	if code != http.StatusBadRequest || h.dials.Load() != 0 {
		t.Fatalf("status %d after %d dials, want 400 and none", code, h.dials.Load())
	}
	if d := h.denials(); len(d) != 1 || d[0] != (denial{"", "malformed request"}) {
		t.Errorf("denials %v", d)
	}
}

func TestAMixedCaseRequestMatchesTheList(t *testing.T) {
	origin := echoOrigin(t)
	h := start(t, []string{"example.com"}, []netip.Addr{publicAddr}, origin, nil)
	if code, _, _ := send(t, h.proxy, "CONNECT Example.COM:443 HTTP/1.1\r\n\r\n"); code != http.StatusOK {
		t.Fatalf("status %d; DNS names compare without case", code)
	}
}

func TestANonPublicAnswerIsRefused(t *testing.T) {
	for _, a := range []string{
		"0.0.0.0", "0.1.2.3", "10.1.2.3", "127.0.0.1", "127.9.9.9", "169.254.169.254", "172.16.0.1", "172.31.255.255",
		"192.168.1.1", "100.64.0.1", "100.127.255.254", "192.0.0.8", "192.0.2.1", "198.18.0.1", "198.19.255.255",
		"198.51.100.7", "203.0.113.7", "224.0.0.1", "239.255.255.250", "240.0.0.1", "255.255.255.255",
		"::", "::1", "fc00::1", "fd12:3456::1", "fe80::1", "fec0::1", "ff02::1", "2001:db8::1", "100::1",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254", "::7f00:1",
		"2002:7f00:1::1", "2002:a00:1::1", "2002:a9fe:a9fe::1",
		"64:ff9b::7f00:1", "64:ff9b::a00:1", "64:ff9b:1::1",
	} {
		t.Run(a, func(t *testing.T) {
			bad := netip.MustParseAddr(a)
			if Public(bad) {
				t.Fatalf("Public(%s) = true", a)
			}
			// One bad answer among good ones refuses the whole name.
			h := start(t, []string{"example.com"}, []netip.Addr{publicAddr, bad}, "127.0.0.1:1", nil)
			code, _, _ := send(t, h.proxy, "CONNECT example.com:443 HTTP/1.1\r\n\r\n")
			if code != http.StatusForbidden || h.dials.Load() != 0 {
				t.Fatalf("status %d after %d dials, want 403 and none", code, h.dials.Load())
			}
			if d := h.denials(); len(d) != 1 || d[0].reason != "non-public address" {
				t.Errorf("denials %v", d)
			}
		})
	}
}

func TestPublicAddressesPass(t *testing.T) {
	for _, a := range []string{"93.184.216.34", "1.1.1.1", "2606:4700::1111", "::ffff:93.184.216.34", "64:ff9b::5db8:d822", "2002:5db8:d822::1"} {
		if !Public(netip.MustParseAddr(a)) {
			t.Errorf("Public(%s) = false", a)
		}
	}
}

func TestAResolverFailureIsRefused(t *testing.T) {
	h := start(t, []string{"example.com"}, []netip.Addr{publicAddr}, "127.0.0.1:1", func(o *Options) {
		o.Resolve = func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("no such host") }
	})
	code, _, _ := send(t, h.proxy, "CONNECT example.com:443 HTTP/1.1\r\n\r\n")
	if code != http.StatusBadGateway || h.dials.Load() != 0 {
		t.Fatalf("status %d after %d dials, want 502 and none", code, h.dials.Load())
	}
	if d := h.denials(); len(d) != 1 || d[0].reason != "resolve failed" {
		t.Errorf("denials %v", d)
	}
}

func TestOversizedHeadersAreRefused(t *testing.T) {
	h := start(t, []string{"example.com"}, []netip.Addr{publicAddr}, "127.0.0.1:1", nil)
	raw := "CONNECT example.com:443 HTTP/1.1\r\nX-Pad: " + strings.Repeat("a", MaxHeaderBytes) + "\r\n\r\n"
	code, _, _ := send(t, h.proxy, raw)
	if code != http.StatusRequestHeaderFieldsTooLarge || h.dials.Load() != 0 {
		t.Fatalf("status %d after %d dials, want 431 and none", code, h.dials.Load())
	}
	if d := h.denials(); len(d) != 1 || d[0].reason != "header too large" {
		t.Errorf("denials %v", d)
	}
}

func TestASlowDialIsBounded(t *testing.T) {
	h := start(t, []string{"example.com"}, []netip.Addr{publicAddr}, "127.0.0.1:1", func(o *Options) {
		o.ConnectTimeout = 20 * time.Millisecond
		o.Dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
	})
	code, _, _ := send(t, h.proxy, "CONNECT example.com:443 HTTP/1.1\r\n\r\n")
	if code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", code)
	}
	if d := h.denials(); len(d) != 1 || d[0].reason != "dial failed" {
		t.Errorf("denials %v", d)
	}
}

func TestASilentClientIsDropped(t *testing.T) {
	h := start(t, []string{"example.com"}, []netip.Addr{publicAddr}, "127.0.0.1:1", func(o *Options) {
		o.HeaderTimeout = 20 * time.Millisecond
	})
	c, err := net.Dial("tcp", h.proxy.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || isTimeout(err) {
		t.Fatalf("read %v; want the proxy to close a client that sends no header", err)
	}
}

func TestAnIdleTunnelIsClosed(t *testing.T) {
	origin := echoOrigin(t)
	h := start(t, []string{"example.com"}, []netip.Addr{publicAddr}, origin, func(o *Options) {
		o.IdleTimeout = 30 * time.Millisecond
	})
	code, c, br := send(t, h.proxy, "CONNECT example.com:443 HTTP/1.1\r\n\r\n")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := br.ReadByte(); err == nil || isTimeout(err) {
		t.Fatalf("read %v; want the idle tunnel closed", err)
	}
}

func TestCloseEndsOpenTunnels(t *testing.T) {
	origin := echoOrigin(t)
	h := start(t, []string{"example.com"}, []netip.Addr{publicAddr}, origin, nil)
	// Bytes sent in the same write as the request reach the origin too.
	code, c, br := send(t, h.proxy, "CONNECT example.com:443 HTTP/1.1\r\n\r\nping\n")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if line, err := br.ReadString('\n'); err != nil || line != "ping\n" {
		t.Fatalf("echo %q, %v", line, err)
	}
	if err := h.proxy.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := br.ReadByte(); err == nil || isTimeout(err) {
		t.Fatalf("read %v after Close; want the tunnel ended", err)
	}
	if c2, err := net.DialTimeout("tcp", h.proxy.Addr(), time.Second); err == nil {
		c2.Close()
		t.Fatal("the proxy still accepts after Close")
	}
}

func TestTheContextStopsTheProxy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p, err := Start(ctx, Options{Hosts: []string{"example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	p.Wait()
	if c, err := net.DialTimeout("tcp", p.Addr(), time.Second); err == nil {
		c.Close()
		t.Fatal("the proxy still accepts after its context ended")
	}
}

func TestStartRefusesABadList(t *testing.T) {
	if _, err := Start(context.Background(), Options{Hosts: []string{"*.example.com"}}); err == nil {
		t.Fatal("Start took a wildcard")
	}
	if _, err := Start(context.Background(), Options{}); err == nil {
		t.Fatal("Start took an empty list")
	}
}

func TestCheckHost(t *testing.T) {
	for _, h := range []string{"example.com", "api.anthropic.com", "a-b.example.co.uk", "xn--bcher-kva.example", "a1.b2"} {
		if err := CheckHost(h); err != nil {
			t.Errorf("CheckHost(%q) = %v", h, err)
		}
	}
	for h, want := range map[string]string{
		"":                                "empty",
		"*.example.com":                   "wildcard",
		"example.*":                       "wildcard",
		"https://example.com":             "scheme",
		"example.com:443":                 "port",
		"10.0.0.0/8":                      "CIDR",
		"example.com/path":                "CIDR",
		"10.1.2.3":                        "IP literal",
		"::1":                             "IP literal",
		"[::1]":                           "IP literal",
		"127.1":                           "IP literal",
		"0x7f.1":                          "IP literal",
		"example.com.":                    "trailing dot",
		"Example.com":                     "uppercase",
		"localhost":                       "at least two labels",
		"exa mple.com":                    "letters, digits",
		"exa_mple.com":                    "letters, digits",
		"-example.com":                    "hyphen",
		"example-.com":                    "hyphen",
		"example..com":                    "empty label",
		strings.Repeat("a", 64) + ".com":  "63",
		strings.Repeat("a.", 127) + "com": "253",
	} {
		err := CheckHost(h)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("CheckHost(%q) = %v, want an error naming %q", h, err, want)
		}
	}
}

func TestValidateHostsRefusesDuplicates(t *testing.T) {
	if err := ValidateHosts([]string{"example.com", "api.example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateHosts([]string{"example.com", "example.com"}); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("duplicate: %v", err)
	}
	if err := ValidateHosts([]string{"example.com", "bad host"}); err == nil || !strings.Contains(err.Error(), `"bad host"`) {
		t.Fatalf("bad entry: %v", err)
	}
}

// echoOrigin is a loopback TCP server that echoes what it reads, closing when the test ends.
func echoOrigin(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return ln.Addr().String()
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func TestTheNextCheckedAddressIsDialledWhenOneFails(t *testing.T) {
	origin := echoOrigin(t)
	second := netip.MustParseAddr("1.1.1.1")
	var tried []string
	h := start(t, []string{"example.com"}, []netip.Addr{publicAddr, second}, origin, func(o *Options) {
		o.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			tried = append(tried, addr)
			if addr != "1.1.1.1:443" {
				return nil, errors.New("unreachable")
			}
			var d net.Dialer
			return d.DialContext(ctx, network, origin)
		}
	})
	if code, _, _ := send(t, h.proxy, "CONNECT example.com:443 HTTP/1.1\r\n\r\n"); code != http.StatusOK {
		t.Fatalf("status %d after dialling %v", code, tried)
	}
	if len(tried) != 2 || tried[0] != "93.184.216.34:443" {
		t.Errorf("dialled %v, want the first address then the second", tried)
	}
}

func TestClosingDuringALookupRecordsNoRefusal(t *testing.T) {
	looking := make(chan struct{})
	h := start(t, []string{"example.com"}, []netip.Addr{publicAddr}, "127.0.0.1:1", func(o *Options) {
		o.Resolve = func(ctx context.Context, _ string) ([]netip.Addr, error) {
			close(looking)
			<-ctx.Done()
			return nil, ctx.Err()
		}
	})
	c, err := net.Dial("tcp", h.proxy.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "CONNECT example.com:443 HTTP/1.1\r\n\r\n")
	<-looking
	h.proxy.Close()
	if d := h.denials(); len(d) != 0 {
		t.Errorf("an allowed host was recorded as refused at shutdown: %v", d)
	}
}

func TestStartWithAnEndedContextGivesAStoppedProxy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p, err := Start(ctx, Options{Hosts: []string{"example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	p.Wait()
	if c, err := net.DialTimeout("tcp", p.Addr(), time.Second); err == nil {
		c.Close()
		t.Fatal("the proxy accepts although its context had ended")
	}
}

// flakyListener fails its first Accept the way a process out of file handles does.
type flakyListener struct {
	net.Listener
	failed atomic.Bool
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if !l.failed.Swap(true) {
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: errors.New("too many open files")}
	}
	return l.Listener.Accept()
}

func TestAFailedAcceptDoesNotStopTheProxy(t *testing.T) {
	listen = func(network, addr string) (net.Listener, error) {
		ln, err := net.Listen(network, addr)
		return &flakyListener{Listener: ln}, err
	}
	t.Cleanup(func() { listen = net.Listen })
	origin := echoOrigin(t)
	h := start(t, []string{"example.com"}, []netip.Addr{publicAddr}, origin, nil)
	if code, _, _ := send(t, h.proxy, "CONNECT example.com:443 HTTP/1.1\r\n\r\n"); code != http.StatusOK {
		t.Fatalf("status %d after a failed accept", code)
	}
}
