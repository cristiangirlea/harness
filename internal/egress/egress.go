// Package egress is an allowlisting HTTPS proxy for one run's agent CLI.
// It is advisory: a process that ignores the proxy variables is not stopped.
package egress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// MaxHeaderBytes bounds a request's line and headers together.
const MaxHeaderBytes = 16 << 10

// Options configures one proxy; zero durations and nil functions take the defaults.
type Options struct {
	// Hosts are the exact DNS names a CONNECT may name; see CheckHost.
	Hosts []string
	// Resolve answers a name's addresses; the default is the system resolver.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// Dial connects to the checked address; the default is a plain TCP dial.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// OnDeny hears each refusal: the target as the client named it and a short reason, never request contents.
	OnDeny func(target, reason string)

	HeaderTimeout  time.Duration // default 10s
	ConnectTimeout time.Duration // resolve and dial; default 10s
	IdleTimeout    time.Duration // a tunnel with no bytes either way; default 5m
}

// Proxy is a running proxy; Close stops it and ends its tunnels.
type Proxy struct {
	o      Options
	ln     net.Listener
	allow  map[string]bool
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

// Start listens on 127.0.0.1 on a free port; the proxy stops when ctx ends or Close is called.
func Start(ctx context.Context, o Options) (*Proxy, error) {
	if len(o.Hosts) == 0 {
		return nil, errors.New("egress: no hosts allowed")
	}
	if err := ValidateHosts(o.Hosts); err != nil {
		return nil, err
	}
	if o.Resolve == nil {
		o.Resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	if o.Dial == nil {
		var d net.Dialer
		o.Dial = d.DialContext
	}
	if o.OnDeny == nil {
		o.OnDeny = func(string, string) {}
	}
	o.HeaderTimeout = orDefault(o.HeaderTimeout, 10*time.Second)
	o.ConnectTimeout = orDefault(o.ConnectTimeout, 10*time.Second)
	o.IdleTimeout = orDefault(o.IdleTimeout, 5*time.Minute)
	ln, err := listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("egress: %w", err)
	}
	p := &Proxy{o: o, ln: ln, allow: map[string]bool{}, conns: map[net.Conn]struct{}{}}
	for _, h := range o.Hosts {
		p.allow[h] = true
	}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	stop := context.AfterFunc(ctx, func() { p.shutdown() })
	p.wg.Go(func() {
		defer stop()
		p.serve()
	})
	return p, nil
}

// listen is replaced in tests to make Accept fail.
var listen = net.Listen

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// Addr is the listening address, 127.0.0.1:port.
func (p *Proxy) Addr() string { return p.ln.Addr().String() }

// URL is the value for HTTPS_PROXY and its kin.
func (p *Proxy) URL() string { return "http://" + p.Addr() }

// Close stops accepting, ends every open connection and waits for them.
func (p *Proxy) Close() error {
	err := p.shutdown()
	p.wg.Wait()
	return err
}

func (p *Proxy) shutdown() error {
	var err error
	p.once.Do(func() {
		p.cancel()
		err = p.ln.Close()
		p.mu.Lock()
		p.closed = true
		for c := range p.conns {
			c.Close()
		}
		p.mu.Unlock()
	})
	return err
}

// Wait returns once the proxy has stopped and its connections are gone.
func (p *Proxy) Wait() { p.wg.Wait() }

// track registers c so Close can end it; false means the proxy already closed it.
func (p *Proxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		c.Close()
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *Proxy) untrack(c net.Conn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
	c.Close()
}

func (p *Proxy) serve() {
	var delay time.Duration
	for {
		c, err := p.ln.Accept()
		if err != nil {
			if p.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			// Out of file handles is passing; stopping would refuse the rest of the run's connections.
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			select {
			case <-p.ctx.Done():
				return
			case <-time.After(delay):
			}
			continue
		}
		delay = 0
		if !p.track(c) {
			continue
		}
		p.wg.Go(func() {
			defer p.untrack(c)
			p.handle(c)
		})
	}
}

var errHeaderTooLarge = errors.New("header too large")

// headerLimit fails reads once n bytes have gone, so a header cannot grow without bound.
type headerLimit struct {
	r io.Reader
	n int
}

func (l *headerLimit) Read(b []byte) (int, error) {
	if l.n <= 0 {
		return 0, errHeaderTooLarge
	}
	if len(b) > l.n {
		b = b[:l.n]
	}
	n, err := l.r.Read(b)
	l.n -= n
	return n, err
}

func (p *Proxy) handle(c net.Conn) {
	c.SetReadDeadline(time.Now().Add(p.o.HeaderTimeout))
	lim := &headerLimit{r: c, n: MaxHeaderBytes}
	br := bufio.NewReaderSize(lim, 4096)
	req, err := http.ReadRequest(br)
	if err != nil {
		// A client that went away or timed out hears nothing; a request the parser refused hears why.
		var op *net.OpError
		switch {
		case lim.n <= 0:
			p.deny(c, "", "header too large", http.StatusRequestHeaderFieldsTooLarge)
		case !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.As(err, &op):
			p.deny(c, "", "malformed request", http.StatusBadRequest)
		}
		return
	}
	if req.Method != http.MethodConnect {
		p.deny(c, capped(req.Host), "not CONNECT", http.StatusForbidden)
		return
	}
	target := capped(req.RequestURI)
	host, port, err := net.SplitHostPort(req.RequestURI)
	if err != nil {
		p.deny(c, target, "no port", http.StatusForbidden)
		return
	}
	if port != "443" {
		p.deny(c, target, "port not 443", http.StatusForbidden)
		return
	}
	if _, err := netip.ParseAddr(host); err == nil {
		p.deny(c, target, "IP literal", http.StatusForbidden)
		return
	}
	host = strings.ToLower(host)
	if !p.allow[host] {
		p.deny(c, target, "not allowed", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(p.ctx, p.o.ConnectTimeout)
	defer cancel()
	addrs, err := p.o.Resolve(ctx, host)
	if p.ctx.Err() != nil {
		return // stopping, not refusing
	}
	if err != nil || len(addrs) == 0 {
		p.deny(c, target, "resolve failed", http.StatusBadGateway)
		return
	}
	// One non-public answer refuses the name: a rebinding resolver may hand it out next.
	if slices.ContainsFunc(addrs, func(a netip.Addr) bool { return !Public(a) }) {
		p.deny(c, target, "non-public address", http.StatusForbidden)
		return
	}
	var up net.Conn
	for _, a := range addrs {
		if up, err = p.o.Dial(ctx, "tcp", net.JoinHostPort(a.WithZone("").String(), "443")); err == nil {
			break
		}
	}
	if up == nil {
		if p.ctx.Err() != nil {
			return
		}
		p.deny(c, target, "dial failed", http.StatusBadGateway)
		return
	}
	if !p.track(up) {
		return
	}
	defer p.untrack(up)
	cancel()
	c.SetDeadline(time.Time{})
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	// Bytes the client sent after the request are already in the reader.
	if n := br.Buffered(); n > 0 {
		early, _ := br.Peek(n)
		if _, err := up.Write(early); err != nil {
			return
		}
	}
	p.tunnel(c, up)
}

// tunnel copies both ways until either side ends or neither has moved a byte for IdleTimeout.
func (p *Proxy) tunnel(a, b net.Conn) {
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 32<<10)
		for {
			src.SetReadDeadline(time.Now().Add(p.o.IdleTimeout))
			n, err := src.Read(buf)
			if n > 0 {
				last.Store(time.Now().UnixNano())
				dst.SetWriteDeadline(time.Now().Add(p.o.IdleTimeout))
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() && time.Since(time.Unix(0, last.Load())) < p.o.IdleTimeout {
					continue
				}
				return
			}
		}
	}
	go pipe(b, a)
	go pipe(a, b)
	<-done
	a.Close()
	b.Close()
	<-done
}

// deny answers status, reports the refusal and lets the client read the answer before the close.
func (p *Proxy) deny(c net.Conn, target, reason string, status int) {
	p.o.OnDeny(target, reason)
	fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", status, http.StatusText(status))
	// Closing with unread input sends a reset that can overtake the answer.
	if tc, ok := c.(*net.TCPConn); ok {
		tc.CloseWrite()
		c.SetReadDeadline(time.Now().Add(time.Second))
		io.CopyN(io.Discard, c, MaxHeaderBytes)
	}
}

// capped keeps a client-chosen target short and printable: it reaches the operator's terminal.
func capped(s string) string {
	if len(s) > 256 {
		s = s[:256]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return '?'
		}
		return r
	}, s)
}
