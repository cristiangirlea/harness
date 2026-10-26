package watch

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestLoopbackAddrAcceptsOnlyLoopback(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:8089":        "127.0.0.1:8089",
		"localhost:0":           "127.0.0.1:0",
		"[::1]:9000":            "[::1]:9000",
		"127.0.0.2:80":          "127.0.0.2:80",
		"[::ffff:127.0.0.1]:80": "127.0.0.1:80",
	} {
		if got, err := LoopbackAddr(in); err != nil || got != want {
			t.Errorf("LoopbackAddr(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"0.0.0.0:8089", ":8089", "[::]:8089", "192.168.1.10:8089", "10.0.0.1:80",
		"example.com:80", "localhost.example.com:80", "8089", "127.0.0.1", "127.0.0.1:99999",
		"127.0.0.1:http", "127.0.0.1:-1", "[fe80::1%eth0]:80", "[::1%lo]:80",
	} {
		if got, err := LoopbackAddr(in); err == nil {
			t.Errorf("LoopbackAddr(%q) = %q; want it refused", in, got)
		}
	}
}

// addrListener is a listener that reports another address than the one it
// listens on.
type addrListener struct {
	net.Listener
	addr net.Addr
}

func (l addrListener) Addr() net.Addr { return l.addr }

func TestServeRefusesAListenerThatIsNotLoopback(t *testing.T) {
	// Serve judges a listener by its address. Listening on every interface for
	// real would make Windows ask about the firewall on each run, since the
	// test binary's path changes every build; a loopback listener that says
	// it is on every interface is judged the same way.
	lo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lo.Close()
	ln := addrListener{lo, &net.TCPAddr{IP: net.IPv4zero, Port: lo.Addr().(*net.TCPAddr).Port}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := Serve(ctx, Options{}, ln); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("Serve on %s = %v; want it refused", ln.Addr(), err)
	}
}

func TestOnlyLoopbackAndTheListedPrivateAddressesMayServe(t *testing.T) {
	lan := []Endpoint{{Addr: "192.168.1.20:8089"}, {Addr: "8.8.8.8:8089"}}
	for _, tc := range []struct {
		addr string
		lan  []Endpoint
		ok   bool
	}{
		{"127.0.0.1:8089", nil, true},
		{"[::1]:8089", nil, true},
		{"192.168.1.20:8089", nil, false},
		{"192.168.1.20:8089", lan, true},
		{"192.168.1.21:8089", lan, false},
		{"8.8.8.8:8089", lan, false}, // listed, but not a private address
		{"0.0.0.0:8089", lan, false},
	} {
		addr, err := net.ResolveTCPAddr("tcp", tc.addr)
		if err != nil {
			t.Fatal(err)
		}
		if err := checkListener(addr, tc.lan); (err == nil) != tc.ok {
			t.Errorf("checkListener(%s, %d listed) = %v; want allowed=%v", tc.addr, len(tc.lan), err, tc.ok)
		}
	}
}

func TestPrivateAddrsKeepsOnlyPrivateIPv4(t *testing.T) {
	var addrs []net.Addr
	for _, cidr := range []string{"192.168.1.20/24", "10.0.0.7/8", "172.20.1.2/16", "127.0.0.1/8", "8.8.8.8/32",
		"169.254.10.1/16", "fd00::1/64", "fe80::1/64", "100.64.0.1/10"} {
		ip, n, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatal(err)
		}
		n.IP = ip
		addrs = append(addrs, n)
	}
	got := privateAddrs(addrs)
	if strings.Join(got, ",") != "192.168.1.20,10.0.0.7,172.20.1.2" {
		t.Fatalf("privateAddrs = %v", got)
	}
}

func TestServeAnswersOnEachListenerUntilItsContextEnds(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	var lns []net.Listener
	for range 2 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		lns = append(lns, ln)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, Options{}, lns...) }()
	for _, ln := range lns {
		if code, _ := get(t, "http://"+ln.Addr().String()+"/"); code != 200 {
			t.Errorf("%s: status %d", ln.Addr(), code)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve = %v after its context ended", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
}

func TestRequestsFromOutsideAreRefused(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	h := New(context.Background(), Options{})
	for _, tc := range []struct {
		name   string
		host   string
		header map[string]string
		want   int
	}{
		{"loopback", "127.0.0.1:8089", nil, 200},
		{"localhost", "localhost:8089", nil, 200},
		{"ipv6 loopback", "[::1]:8089", nil, 200},
		{"rebound name", "evil.example:8089", nil, 403},
		{"lan address", "192.168.1.5:8089", nil, 403},
		{"no host", "", nil, 403},
		{"cross-site fetch", "127.0.0.1:8089", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"cross-site navigation", "127.0.0.1:8089", map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "cross-site"}, 403},
		{"foreign origin", "127.0.0.1:8089", map[string]string{"Origin": "http://evil.example"}, 403},
		{"own origin", "127.0.0.1:8089", map[string]string{"Origin": "http://127.0.0.1:8089"}, 200},
	} {
		req := httptest.NewRequest("GET", "/", nil)
		req.Host = tc.host
		for k, v := range tc.header {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, rec.Code, tc.want)
		}
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
			t.Errorf("%s: CSP = %q", tc.name, csp)
		}
	}
	// With --lan, the page's own private addresses are let in, and only those.
	// A browser leaves port 80 out of Host.
	lan := New(context.Background(), Options{LAN: []Endpoint{{Addr: "192.168.1.20:8089"}, {Addr: "192.168.1.30:80"}}})
	for host, want := range map[string]int{"192.168.1.20:8089": 200, "192.168.1.20:9999": 403, "192.168.1.21:8089": 403, "127.0.0.1:8089": 200, "evil.example:8089": 403,
		"192.168.1.30": 200, "192.168.1.30:80": 200, "192.168.1.20": 403} {
		req := httptest.NewRequest("GET", "/", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		lan.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("--lan, Host %s: status %d, want %d", host, rec.Code, want)
		}
	}
}

// Anything but GET and HEAD would act on hrn. It must carry hrn watch's key,
// from this computer or the network alike; there is no action yet, so a keyed
// request gets as far as the router and is refused there.
func TestAnythingThatWouldActNeedsTheKey(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, tc := range []struct {
		name, key, sent string
		want            int
	}{
		{"no key set", "", "", 403},
		{"no key sent", "k3y", "", 403},
		{"wrong key", "k3y", "nope", 403},
		{"right key", "k3y", "k3y", http.StatusMethodNotAllowed},
	} {
		req := httptest.NewRequest("POST", "/", nil)
		req.Host, req.RemoteAddr = "127.0.0.1:8089", "127.0.0.1:5000"
		if tc.sent != "" {
			req.Header.Set("X-Hrn-Watch-Key", tc.sent)
		}
		rec := httptest.NewRecorder()
		New(context.Background(), Options{Key: tc.key}).ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
}

// The door by method, origin and where the request comes from: viewing passes
// without a key, everything else needs it, and the key does not lift the
// other checks.
func TestTheDoorLetsViewingThroughAndHoldsEverythingElse(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	h := New(context.Background(), Options{Key: "k3y", LAN: []Endpoint{{Addr: "192.168.1.20:8089"}}})
	for _, tc := range []struct {
		name, method, host, remote, key string
		header                          map[string]string
		want                            int
	}{
		{"GET, no key", "GET", "127.0.0.1:8089", "127.0.0.1:5000", "", nil, 200},
		{"HEAD, no key", "HEAD", "127.0.0.1:8089", "127.0.0.1:5000", "", nil, 200},
		{"GET from the network, no key", "GET", "192.168.1.20:8089", "192.168.1.50:6000", "", nil, 200},
		{"OPTIONS, no key", "OPTIONS", "127.0.0.1:8089", "127.0.0.1:5000", "", nil, 403},
		{"POST from the network, no key", "POST", "192.168.1.20:8089", "192.168.1.50:6000", "", nil, 403},
		{"POST from the network, key", "POST", "192.168.1.20:8089", "192.168.1.50:6000", "k3y", nil, http.StatusMethodNotAllowed},
		{"POST cross-site with the key", "POST", "127.0.0.1:8089", "127.0.0.1:5000", "k3y", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"POST from a foreign origin with the key", "POST", "127.0.0.1:8089", "127.0.0.1:5000", "k3y", map[string]string{"Origin": "http://evil.example"}, 403},
	} {
		req := httptest.NewRequest(tc.method, "/", nil)
		req.Host, req.RemoteAddr = tc.host, tc.remote
		if tc.key != "" {
			req.Header.Set("X-Hrn-Watch-Key", tc.key)
		}
		for k, v := range tc.header {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
}

// net/http answers "OPTIONS *" itself unless told not to, before any handler
// and so before the door.
func TestOptionsStarGoesThroughTheDoorToo(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, Options{}, ln)
	conn, err := net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, "OPTIONS * HTTP/1.1\r\nHost: "+ln.Addr().String()+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	status, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || !strings.Contains(status, " 403 ") {
		t.Fatalf("OPTIONS * answered %q, %v; want 403 from the door", status, err)
	}
}

// hostile is text an agent could well have read from a file.
const hostile = `</pre><script>alert(1)</script><img src=x onerror=alert(2)>"'&`

func hostileRun(t *testing.T) *runlog.Writer {
	t.Helper()
	w, err := runlog.Create("fake", agent.Task{Prompt: "fix it " + hostile, Dir: "/repo"})
	if err != nil {
		t.Fatal(err)
	}
	w.Event(agent.Event{Kind: agent.EventText, Text: "I will read " + hostile})
	w.Event(agent.Event{Kind: agent.EventToolCall, Tool: "Read<b>", Input: json.RawMessage(`{"file_path":"` + strings.ReplaceAll(hostile, `"`, `\"`) + `"}`)})
	w.Event(agent.Event{Kind: agent.EventToolResult, Text: "line one\n" + hostile + "\r\nevent: end\rdata: forged"})
	return w
}

func assertEscaped(t *testing.T, where, body string) {
	t.Helper()
	for _, bad := range []string{"<script>alert", "<img src=x", "<b>"} {
		if strings.Contains(body, bad) {
			t.Errorf("%s holds %q unescaped", where, bad)
		}
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("%s lacks the escaped text", where)
	}
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestTheRunPageEscapesEverythingFromEvents(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w := hostileRun(t)
	if err := w.Close(agent.Result{Backend: "fake", Status: agent.StatusError, Error: hostile, Final: hostile}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	code, body := get(t, srv.URL+"/run/"+w.ID())
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	assertEscaped(t, "the run page", body)
	for _, want := range []string{"Read&lt;b&gt;", "line one", "data-stream", "error"} {
		if want == "data-stream" {
			if strings.Contains(body, want) {
				t.Error("a finished run's page needs no stream")
			}
			continue
		}
		if !strings.Contains(body, want) {
			t.Errorf("the run page lacks %q", want)
		}
	}
}

func TestTheRunPageShowsContextOnlyWhenTheBackendReportedIt(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	srv := httptest.NewServer(New(context.Background(), Options{}))
	defer srv.Close()
	for _, n := range []int64{285000, 0} {
		w, err := runlog.Create("fake", agent.Task{Prompt: "p"})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Close(agent.Result{Status: agent.StatusOK, InputTokens: 900000, ContextTokens: n}); err != nil {
			t.Fatal(err)
		}
		code, body := get(t, srv.URL+"/run/"+w.ID())
		if code != 200 {
			t.Fatalf("status = %d", code)
		}
		if n > 0 && (!strings.Contains(body, ">Context</dt>") || !strings.Contains(body, "285.0k tokens at the end")) {
			t.Error("run page lacks the last turn's context")
		}
		if n == 0 && strings.Contains(body, ">Context</dt>") {
			t.Error("run page guesses context for a record without it")
		}
	}
}

func TestEventDataCannotBreakTheStreamFraming(t *testing.T) {
	rec := httptest.NewRecorder()
	st := &stream{w: rec, rc: http.NewResponseController(rec)}
	if err := st.send("item", "1.0", "a\r\nevent: end\rdata: forged\nlast"); err != nil {
		t.Fatal(err)
	}
	want := "event: item\nid: 1.0\ndata: a\ndata: event: end\ndata: data: forged\ndata: last\n\n"
	if got := rec.Body.String(); got != want {
		t.Fatalf("frame = %q\nwant    %q", got, want)
	}
}

// sse reads a Server-Sent Events stream one message at a time.
type sse struct {
	r *bufio.Reader
}

type message struct{ event, id, data string }

func (s sse) next(t *testing.T) (message, bool) {
	t.Helper()
	var m message
	var data []string
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			return m, false
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case line == "":
			if m.event == "" && data == nil {
				continue // a keep-alive comment or the retry line
			}
			m.data = strings.Join(data, "\n")
			return m, true
		case strings.HasPrefix(line, ":"), strings.HasPrefix(line, "retry:"):
		case strings.HasPrefix(line, "event: "):
			m.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			m.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			data = append(data, strings.TrimPrefix(line, "data: "))
		default:
			t.Fatalf("unexpected stream line %q", line)
		}
	}
}

func openStream(t *testing.T, url, lastID string) (sse, func()) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	return sse{bufio.NewReader(resp.Body)}, func() { resp.Body.Close() }
}

func TestTheRunStreamFollowsARunUntilItFinishes(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w := hostileRun(t)
	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()

	code, page := get(t, srv.URL+"/run/"+w.ID())
	if code != 200 || !strings.Contains(page, `data-stream="/stream/run/`+w.ID()+`?after=`) {
		t.Fatalf("a running run's page streams the rest: %d\n%s", code, page)
	}

	s, stop := openStream(t, srv.URL+"/stream/run/"+w.ID(), "")
	defer stop()
	var items []message
	for len(items) < 3 {
		m, ok := s.next(t)
		if !ok {
			t.Fatal("stream ended early")
		}
		if m.event != "item" || m.id == "" {
			t.Fatalf("message = %+v, want an item with an id", m)
		}
		items = append(items, m)
	}
	assertEscaped(t, "the stream", items[0].data+items[1].data+items[2].data)
	if !strings.Contains(items[2].data, "<details") {
		t.Errorf("a tool result is collapsed: %s", items[2].data)
	}

	w.Event(agent.Event{Kind: agent.EventToolCall, Tool: "Bash", Text: "go test ./..."})
	m, ok := s.next(t)
	if !ok || m.event != "item" || !strings.Contains(m.data, "go test ./...") {
		t.Fatalf("the next event = %+v, %v", m, ok)
	}
	if err := w.Close(agent.Result{Backend: "fake", Model: "m-1", Status: agent.StatusOK, Turns: 3,
		InputTokens: 1200, OutputTokens: 340, CostUSD: 0.0123, Duration: 65 * time.Second}); err != nil {
		t.Fatal(err)
	}
	m, ok = s.next(t)
	if !ok || m.event != "result" {
		t.Fatalf("after the run ended: %+v, %v; want its result", m, ok)
	}
	for _, want := range []string{"ok", "m-1", "1.2k", "340", "$0.0123", "1m05s"} {
		if !strings.Contains(m.data, want) {
			t.Errorf("result lacks %q:\n%s", want, m.data)
		}
	}
	if m, ok = s.next(t); !ok || m.event != "end" {
		t.Fatalf("then %+v, %v; want end", m, ok)
	}
	if m, ok = s.next(t); ok {
		t.Fatalf("the stream went on after its end: %+v", m)
	}

	// A browser that reconnects sends the last id it saw and gets the rest.
	s2, stop2 := openStream(t, srv.URL+"/stream/run/"+w.ID(), items[2].id)
	defer stop2()
	m, ok = s2.next(t)
	if !ok || m.event != "item" || !strings.Contains(m.data, "go test ./...") {
		t.Fatalf("after a reconnect: %+v, %v; want only the event not yet seen", m, ok)
	}
	if !strings.Contains(m.data, `class="gap`) {
		t.Errorf("the time since the step before survives a reconnect: %s", m.data)
	}
}

func TestTheRunStreamRefusesABadCursorAndAMissingRun(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w := hostileRun(t)
	defer w.Close(agent.Result{})
	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	if code, _ := get(t, srv.URL+"/stream/run/"+w.ID()+"?after=nonsense"); code != 400 {
		t.Errorf("bad cursor: %d, want 400", code)
	}
	if code, _ := get(t, srv.URL+"/stream/run/20991231-235959-abcdef"); code != 404 {
		t.Errorf("missing run: %d, want 404", code)
	}
}

// Ids from a request become file names. Records planted where an unchecked id
// would lead show that the check, not a missing file, refuses them; CON is a
// device on Windows, and reading it would wait on hrn's console.
func TestIdsThatAreNotHrnsOwnAreRefused(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	w, err := runlog.Create("fake", agent.Task{Prompt: "planted"})
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Close(agent.Result{Status: agent.StatusOK})
	if err := os.Rename(w.Dir(), filepath.Join(home, "escape")); err != nil {
		t.Fatal(err)
	}
	saveBatch(t, batch.Result{ID: "planted", Duration: time.Second})
	if err := os.Rename(filepath.Join(home, "batches", "planted.json"), filepath.Join(home, "escape.json")); err != nil {
		t.Fatal(err)
	}
	saveBatch(t, batch.Result{ID: "nonstandard", Duration: time.Second})

	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	for _, path := range []string{"/run/..%5Cescape", "/stream/run/..%5Cescape", "/batch/..%5Cescape", "/stream/batch/..%5Cescape",
		"/batch/nonstandard", "/batch/CON", "/stream/batch/CON", "/run/CON", "/run/NUL"} {
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Errorf("%s: status %d, want 404", path, resp.StatusCode)
		}
	}
}

func saveBatch(t *testing.T, res batch.Result) {
	t.Helper()
	dir := filepath.Join(runlog.Home(), "batches")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(res)
	if err := os.WriteFile(filepath.Join(dir, res.ID+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTheBatchPageShowsEachTasksRunAndGates(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	res := batch.Result{ID: "20261010-120000-abcdef", Started: time.Now().Add(-time.Minute), Dir: "/repo", Base: "0123456789abcdef", Parallel: 2,
		Tasks: []batch.TaskResult{
			{ID: "t1", Prompt: "first line\nsecond line", Status: batch.StatusFail, Branch: "hrn/t1", Attempts: []batch.Attempt{{
				Backend: "claude", RunID: "20261010-120001-111111", Result: agent.Result{Status: agent.StatusOK, Turns: 4},
				Gates: []batch.GateResult{
					{Command: "go vet ./...", ExitCode: 0, Duration: time.Second},
					{Command: "go test ./...", ExitCode: 1, Output: "--- FAIL " + hostile, Duration: 3 * time.Second},
				},
			}}},
			{ID: "t2", Prompt: "other", Status: batch.StatusRunning, Branch: "hrn/t2", Attempts: []batch.Attempt{{
				Backend: "codex", RunID: "20261010-120002-222222",
			}}},
			{ID: "t3", Prompt: "slow", Status: batch.StatusFail, Branch: "hrn/t3", Attempts: []batch.Attempt{{
				Backend: "claude", RunID: "20261010-120003-333333", Result: agent.Result{Status: agent.StatusOK, Turns: 2},
				Gates: []batch.GateResult{{Command: "go test -count=1 ./...", ExitCode: -1, Killed: batch.KilledTimeout, Timeout: agent.Duration(15 * time.Minute), Output: "[timed out after 15m0s]", Duration: 15 * time.Minute}},
			}}},
			{ID: "t4", Prompt: "cut", Status: batch.StatusCanceled, Branch: "hrn/t4", Attempts: []batch.Attempt{{
				Backend: "claude", RunID: "20261010-120004-444444", Result: agent.Result{Status: agent.StatusOK, Turns: 1},
				Gates: []batch.GateResult{{Command: "go vet ./...", ExitCode: -1, Killed: batch.KilledCanceled, Output: "[canceled]", Duration: time.Second}},
			}}},
		}}
	saveBatch(t, res)
	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()

	code, body := get(t, srv.URL+"/batch/"+res.ID)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{`href="/run/20261010-120001-111111"`, `href="/run/20261010-120002-222222"`,
		"go vet ./...", "go test ./...", ">1<", "<details", "first line", "running", `data-stream="/stream/batch/` + res.ID,
		// The badge is short, since .badge and .num never wrap; the full text
		// the batch tool prints is its title.
		`<span class="badge bad" title="killed after 15m0s (gate_timeout; set &#34;gate_timeout&#34; in the spec to allow more)">killed · gate_timeout 15m0s</span>`,
		`<span class="badge bad">canceled</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the batch page lacks %q", want)
		}
	}
	if strings.Contains(body, ">-1<") || strings.Contains(body, ">killed<") || strings.Contains(body, "in the spec to allow more</span>") {
		t.Error("a killed gate shows only its exit code, a bare killed badge, or the full text as the badge")
	}

	if !strings.Contains(body, "--- FAIL &lt;/pre&gt;&lt;script&gt;") || strings.Contains(body, "<script>alert") {
		t.Error("gate output is not escaped")
	}

	// The stream sends the page's live part, then again when the record changes.
	s, stop := openStream(t, srv.URL+"/stream/batch/"+res.ID, "")
	defer stop()
	if m, ok := s.next(t); !ok || m.event != "replace" || !strings.Contains(m.data, "codex") {
		t.Fatalf("first message = %+v, %v", m, ok)
	}
	res.Tasks[1].Status = batch.StatusPass
	res.Tasks[1].Attempts[0].Result = agent.Result{Status: agent.StatusOK}
	res.Tasks[1].Attempts[0].Gates = []batch.GateResult{{Command: "make check", ExitCode: 0}}
	res.Duration = 2 * time.Minute
	saveBatch(t, res)
	m, ok := s.next(t)
	if !ok || m.event != "replace" || !strings.Contains(m.data, "make check") {
		t.Fatalf("after a gate finished: %+v, %v", m, ok)
	}
	if m, ok = s.next(t); !ok || m.event != "end" {
		t.Fatalf("a finished batch ends its stream: %+v, %v", m, ok)
	}
}

func TestTheIndexListsRunsAndBatchesNewestFirst(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	old, err := runlog.Create("claude", agent.Task{Prompt: "older task\nwith more lines"})
	if err != nil {
		t.Fatal(err)
	}
	_ = old.Close(agent.Result{Backend: "claude", Status: agent.StatusOK, Duration: 90 * time.Second, CostUSD: 0.0123})
	time.Sleep(1100 * time.Millisecond) // run ids are ordered by the second they start
	running, err := runlog.Create("codex", agent.Task{Prompt: "\n  newer task " + hostile})
	if err != nil {
		t.Fatal(err)
	}
	defer running.Close(agent.Result{})
	saveBatch(t, batch.Result{ID: "20261010-130000-cccccc", Started: time.Now(), Tasks: []batch.TaskResult{
		{ID: "t1", Prompt: "batch task", Status: batch.StatusPass, Attempts: []batch.Attempt{{Backend: "api", Result: agent.Result{Status: agent.StatusOK, CostUSD: 0.25, CostEstimate: true}}}},
		{ID: "t2", Prompt: "second", Status: batch.StatusRunning, Attempts: []batch.Attempt{{Backend: "api", Result: agent.Result{Status: agent.StatusOK, CostUSD: 0.5, CostEstimate: true}}}},
	}})

	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	code, body := get(t, srv.URL+"/")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	newer, older := strings.Index(body, running.ID()), strings.Index(body, old.ID())
	if newer < 0 || older < 0 || newer > older {
		t.Errorf("runs are not newest first (newer at %d, older at %d)", newer, older)
	}
	if strings.Contains(body, "with more lines") || !strings.Contains(body, "older task") {
		t.Error("a run shows its prompt's first line only")
	}
	for _, want := range []string{"1m30s", "$0.0123", "~$0.7500", "codex", "running", "20261010-130000-cccccc", "batch task, second", `data-stream="/stream/index"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the index lacks %q", want)
		}
	}
	assertEscaped(t, "the index", body)
}

func TestTheIndexStreamSendsANewRunAfterItsFirstReplace(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	srv := httptest.NewServer(New(ctx, Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	s, stop := openStream(t, srv.URL+"/stream/index", "")
	defer stop()

	first, ok := s.next(t)
	if !ok || first.event != "replace" || strings.Contains(first.data, `href="/run/`) {
		t.Fatalf("first message = %+v, %v; want an index without runs", first, ok)
	}
	// Create the run only after the first fragment has arrived, so only a
	// later poll on this same connection can show it.
	const prompt = "a run created after the index stream opened"
	w, err := runlog.Create("fake", agent.Task{Prompt: prompt})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(agent.Result{})
	for {
		m, ok := s.next(t)
		if !ok {
			t.Fatalf("index stream ended before a later replace showed run %s (context: %v)", w.ID(), ctx.Err())
		}
		if m.event != "replace" {
			t.Fatalf("later message = %+v; want replace", m)
		}
		if !strings.Contains(m.data, `href="/run/`+w.ID()+`"`) {
			continue // A poll may have rendered before the new record was readable.
		}
		if row := runRowOf(t, m.data, w.ID()); !strings.Contains(row, prompt) {
			t.Fatalf("later replace lacks the new run's prompt: %s", row)
		}
		break
	}
}

// runRowOf is the index row of one run: from its link to the end of the row.
func runRowOf(t *testing.T, body, id string) string {
	t.Helper()
	i := strings.Index(body, `<a href="/run/`+id+`"`)
	if i < 0 {
		t.Fatalf("the index has no row for run %s", id)
	}
	row, _, _ := strings.Cut(body[i:], "</tr>")
	return row
}

func TestARunABatchStartedLinksToItsBatch(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	newRun := func(prompt string) string {
		t.Helper()
		w, err := runlog.Create("fake", agent.Task{Prompt: prompt})
		if err != nil {
			t.Fatal(err)
		}
		_ = w.Close(agent.Result{Backend: "fake", Status: agent.StatusOK})
		return w.ID()
	}
	x, oldX, y := newRun("batch work"), newRun("old batch work"), newRun("plain run")
	retry, second := newRun("batch work, again"), newRun("second task")
	const id, oldID = "20261010-140000-aaaaaa", "20261001-090000-bbbbbb"
	start := time.Now()
	saveBatch(t, batch.Result{ID: id, Started: start, Tasks: []batch.TaskResult{
		{ID: "t1", Prompt: "batch work", Status: batch.StatusPass, Attempts: []batch.Attempt{
			{Backend: "fake", RunID: x, Result: agent.Result{Status: agent.StatusOK}},
			// An attempt naming a run that has no record: nothing to link, nothing wrong.
			{Backend: "fake", RunID: "20261010-140001-999999", Result: agent.Result{Status: agent.StatusOK}},
			{Backend: "fake", RunID: retry, Result: agent.Result{Status: agent.StatusOK}},
		}},
		{ID: "t2", Prompt: "second task", Status: batch.StatusPass, Attempts: []batch.Attempt{{Backend: "fake", RunID: second}}},
	}})
	// The oldest batch is past the 20 the index lists; its run still links to it.
	saveBatch(t, batch.Result{ID: oldID, Started: start.Add(-time.Hour), Tasks: []batch.TaskResult{
		{ID: "t1", Prompt: "old batch work", Status: batch.StatusPass, Attempts: []batch.Attempt{{Backend: "fake", RunID: oldX}}},
	}})
	for i := range 20 {
		saveBatch(t, batch.Result{ID: fmt.Sprintf("20261005-100000-%06x", i), Started: start.Add(-time.Duration(i+1) * time.Minute)})
	}

	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	code, body := get(t, srv.URL+"/")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if strings.Contains(body, `href="/batch/`+oldID+`">`+oldID+`</a></td>`) {
		t.Fatal("the old batch is in the batches table; the test needs it past the 20 listed")
	}
	// links checks the runs' rows in one rendering of the lists.
	links := func(where, html string) {
		t.Helper()
		// The first attempt, a later attempt of the same task, and another task's.
		for _, run := range []string{x, retry, second} {
			if row := runRowOf(t, html, run); !strings.Contains(row, `href="/batch/`+id+`"`) || !strings.Contains(row, "batch "+id) {
				t.Errorf("%s: run %s, a batch's attempt, does not link to its batch:\n%s", where, run, row)
			}
		}
		if row := runRowOf(t, html, oldX); !strings.Contains(row, `href="/batch/`+oldID+`"`) {
			t.Errorf("%s: run %s of a batch older than the 20 listed does not link to it:\n%s", where, oldX, row)
		}
		if row := runRowOf(t, html, y); strings.Contains(row, "/batch/") {
			t.Errorf("%s: plain run %s links to a batch:\n%s", where, y, row)
		}
	}
	links("the index", body)
	if strings.Contains(body, "20261010-140001-999999") || strings.Contains(body, `class="notice bad"`) {
		t.Error("an attempt's run with no record shows up on the index or adds a problem")
	}
	if !strings.Contains(body, "also appear as runs") {
		t.Error("the intro does not say a batch's tasks also appear as runs")
	}

	s, stop := openStream(t, srv.URL+"/stream/index", "")
	defer stop()
	m, ok := s.next(t)
	if !ok || m.event != "replace" {
		t.Fatalf("first message = %+v, %v", m, ok)
	}
	links("the streamed index", m.data)
}

func TestTheIndexSaysWhereAPhoneCanOpenIt(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	ctx := context.Background()
	rec := httptest.NewRecorder()
	local := httptest.NewRequest("GET", "http://127.0.0.1:8089/", nil)
	local.RemoteAddr = "127.0.0.1:5000"
	New(ctx, Options{}).ServeHTTP(rec, local)
	if body := rec.Body.String(); !strings.Contains(body, "hrn watch --lan") {
		t.Errorf("without --lan the index says how to reach it from the network:\n%s", body)
	}
	// With --lan, the PC's own view lists each phone link with its QR code.
	lan := New(ctx, Options{LAN: []Endpoint{{Addr: "192.168.1.20:8089", Interface: "Wi-Fi", URL: "http://192.168.1.20:8089/"}}})
	req := httptest.NewRequest("GET", "http://127.0.0.1:8089/", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	rec = httptest.NewRecorder()
	lan.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{`href="http://192.168.1.20:8089/"`, "Wi-Fi", "anyone on", `<svg class="qr"`, `<path d="M`} {
		if !strings.Contains(body, want) {
			t.Errorf("with --lan the PC's index lacks %q", want)
		}
	}
	// The phone itself needs neither the links nor the PC's instructions.
	req = httptest.NewRequest("GET", "http://192.168.1.20:8089/", nil)
	req.RemoteAddr = "192.168.1.50:6000"
	rec = httptest.NewRecorder()
	lan.ServeHTTP(rec, req)
	body = rec.Body.String()
	for _, unwanted := range []string{`<svg class="qr"`, "Only this computer", "hrn watch --lan"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("a phone's view of the index shows %q", unwanted)
		}
	}
}

func TestThePagesLoadNothingFromElsewhere(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	srv := httptest.NewServer(New(context.Background(), Options{}))
	defer srv.Close()
	for _, path := range []string{"/", "/static/watch.css", "/static/watch.js"} {
		code, body := get(t, srv.URL+path)
		if code != 200 {
			t.Errorf("%s: status %d", path, code)
		}
		for _, remote := range []string{"http://", "https://", "//fonts.", "@import", "url("} {
			if strings.Contains(body, remote) {
				t.Errorf("%s refers to %q", path, remote)
			}
		}
	}
	if code, _ := get(t, srv.URL+"/static/other.js"); code != 404 {
		t.Errorf("an unknown static file: %d", code)
	}
}

func TestStreamsEndWhenTheServerStops(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(New(ctx, Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	s, stop := openStream(t, srv.URL+"/stream/index", "")
	defer stop()
	if m, ok := s.next(t); !ok || m.event != "replace" {
		t.Fatalf("first message = %+v, %v", m, ok)
	}
	cancel()
	done := make(chan bool, 1)
	go func() {
		_, _ = io.Copy(io.Discard, s.r) // until the server ends the stream
		done <- true
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream outlived the server")
	}
}

func TestARunPageCaughtWhileTheResultIsWrittenKeepsStreaming(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w := hostileRun(t)
	defer w.Close(agent.Result{})
	if err := os.WriteFile(filepath.Join(w.Dir(), "result.json"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	code, body := get(t, srv.URL+"/run/"+w.ID())
	if code != 200 || strings.Contains(body, "Finished") || !strings.Contains(body, `data-stream="/stream/run/`+w.ID()+"?after=") {
		t.Fatalf("status %d; want a running page that streams the rest:\n%s", code, body)
	}
}

// --lan is for the network a phone is on: adapters that are down, loopback
// and point-to-point links (VPN tunnels) are left out.
func TestLanPicksThePrivateAddressesOfAdaptersThatAreUp(t *testing.T) {
	addr := func(cidr string) net.Addr {
		ip, n, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatal(err)
		}
		n.IP = ip
		return n
	}
	got := lanAddrs([]adapter{
		{"Wi-Fi", net.FlagUp, []net.Addr{addr("192.168.1.20/24"), addr("fe80::1/64")}},
		{"Ethernet", 0, []net.Addr{addr("192.168.2.5/24")}},
		{"Loopback", net.FlagUp | net.FlagLoopback, []net.Addr{addr("127.0.0.1/8")}},
		{"WireGuard", net.FlagUp | net.FlagPointToPoint, []net.Addr{addr("10.8.0.2/24")}},
		{"vEthernet (WSL)", net.FlagUp, []net.Addr{addr("172.20.1.1/20")}},
	})
	var names []string
	for _, a := range got {
		names = append(names, a.ip+" "+a.name)
	}
	if strings.Join(names, ", ") != "192.168.1.20 Wi-Fi, 172.20.1.1 vEthernet (WSL)" {
		t.Fatalf("lanAddrs = %v", names)
	}
}

// The page shows the steps written so far and streams from where it stopped:
// a step is never shown twice, nor skipped.
func TestTheRunPageStreamsOnlyWhatItDidNotShow(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w := hostileRun(t)
	defer w.Close(agent.Result{})
	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	_, page := get(t, srv.URL+"/run/"+w.ID())
	_, rest, ok := strings.Cut(page, `data-stream="`)
	if !ok {
		t.Fatal("the page of a running run has no stream")
	}
	url, _, _ := strings.Cut(rest, `"`)
	w.Event(agent.Event{Kind: agent.EventInfo, Text: "the fourth step"})
	s, stop := openStream(t, srv.URL+url, "")
	defer stop()
	m, ok := s.next(t)
	if !ok || m.event != "item" || !strings.Contains(m.data, "the fourth step") {
		t.Fatalf("the stream began with %+v, %v; want the step written after the page", m, ok)
	}
}

// --lan=NAME serves one network: the adapter named (any case) or the address
// given. A name that matches nothing says which there are.
func TestLanCanBeNarrowedToOneAdapter(t *testing.T) {
	all := []lanAddr{{"192.168.1.218", "Ethernet 3"}, {"172.22.176.1", "vEthernet (WSL)"}, {"192.168.28.1", "VMware Network Adapter VMnet1"}}
	for _, tc := range []struct {
		only string
		want string
	}{
		{"", "192.168.1.218,172.22.176.1,192.168.28.1"},
		{"Ethernet 3", "192.168.1.218"},
		{"ethernet 3", "192.168.1.218"},
		{"172.22.176.1", "172.22.176.1"},
	} {
		got, err := pickLAN(all, tc.only)
		var ips []string
		for _, a := range got {
			ips = append(ips, a.ip)
		}
		if err != nil || strings.Join(ips, ",") != tc.want {
			t.Errorf("pickLAN(%q) = %v, %v; want %s", tc.only, ips, err, tc.want)
		}
	}
	_, err := pickLAN(all, "Wi-Fi")
	if err == nil || !strings.Contains(err.Error(), "Ethernet 3 (192.168.1.218)") {
		t.Errorf("an unknown adapter: %v; want the ones there are", err)
	}
}

// A token or key that ended up in a run (an agent ran env, say) is not shown
// to anyone: watch knows the values in its own environment and blanks them.
func TestSecretValuesFromTheEnvironmentAreRedacted(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	const key = "sk-ant-api03-test-0123456789abcdefXYZ"
	t.Setenv("ANTHROPIC_API_KEY", key)
	t.Setenv("SHORT_TOKEN", "abc") // too short to be worth hiding, and would blank common text
	w, err := runlog.Create("fake", agent.Task{Prompt: "show env"})
	if err != nil {
		t.Fatal(err)
	}
	w.Event(agent.Event{Kind: agent.EventToolResult, Text: "PATH=/bin\nANTHROPIC_API_KEY=" + key + "\nabc"})
	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	_, page := get(t, srv.URL+"/run/"+w.ID())
	if strings.Contains(page, key) || !strings.Contains(page, "ANTHROPIC_API_KEY=[redacted]") {
		t.Errorf("the run page shows the key, or no redaction:\n%s", page)
	}
	s, stop := openStream(t, srv.URL+"/stream/run/"+w.ID(), "")
	defer stop()
	w.Event(agent.Event{Kind: agent.EventText, Text: "the key is " + key})
	for {
		m, ok := s.next(t)
		if !ok {
			t.Fatal("stream ended")
		}
		if strings.Contains(m.data, "the key is") {
			if strings.Contains(m.data, key) {
				t.Errorf("the stream shows the key: %s", m.data)
			}
			break
		}
	}
	_ = w.Close(agent.Result{})
}

// The footer names this computer's hrn home, and with it the user name; a
// phone on the network is not told it, nor the paths in an error.
func TestThePhoneIsNotToldTheHomePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	h := New(context.Background(), Options{LAN: []Endpoint{{Addr: "192.168.1.20:8089"}}})
	for _, path := range []string{"/", "/run/20991231-235959-abcdef"} {
		req := httptest.NewRequest("GET", "http://192.168.1.20:8089"+path, nil)
		req.RemoteAddr = "192.168.1.50:6000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if strings.Contains(rec.Body.String(), home) {
			t.Errorf("%s shows the phone the hrn home %s", path, home)
		}
		local := httptest.NewRequest("GET", "http://127.0.0.1:8089"+path, nil)
		local.RemoteAddr = "127.0.0.1:5000"
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, local)
		if path == "/" && !strings.Contains(rec.Body.String(), home) {
			t.Errorf("this computer's view of / should still name the hrn home")
		}
	}
}

// A key cut in two by a page's length cap must not show either half: secrets
// are blanked in what a record says before anything is cut, as well as in
// what the page says.
func TestASecretCutByACapDoesNotShowEitherHalf(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	const key = "sk-ant-api03-Q9w8E7r6T5y4U3i2O1p0-abcdefghijklmnop"
	t.Setenv("ANTHROPIC_API_KEY", key)
	w, err := runlog.Create("fake", agent.Task{Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(agent.Result{})
	head := strings.Repeat("a", resultHead-20)
	w.Event(agent.Event{Kind: agent.EventToolResult, Text: head + key + strings.Repeat("b", 30<<10)})
	w.Event(agent.Event{Kind: agent.EventToolResult, Text: strings.Repeat("c", 30<<10) + key[:30] + key[30:] + strings.Repeat("d", resultTail-25)})
	w.Event(agent.Event{Kind: agent.EventToolResult, Text: strings.Repeat("e", 100) + key})                        // the one-line summary's cut
	w.Event(agent.Event{Kind: agent.EventToolCall, Tool: "Bash", Input: []byte(`{"command":"echo ` + key + `"}`)}) // JSON form
	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	_, page := get(t, srv.URL+"/run/"+w.ID())
	for i := 0; i+16 <= len(key); i += 4 {
		if strings.Contains(page, key[i:i+16]) {
			t.Fatalf("the page shows part of the key: %q", key[i:i+16])
		}
	}
}

// A stream that fails names files in its error; a phone is told only that it
// failed.
func TestAStreamErrorDoesNotTellThePhoneThePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	w, err := runlog.Create("fake", agent.Task{Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(agent.Result{})
	h := New(context.Background(), Options{Poll: 5 * time.Millisecond, LAN: []Endpoint{{Addr: "192.168.1.20:8089"}}})
	for _, remote := range []string{"192.168.1.50:6000", "127.0.0.1:5000"} {
		req := httptest.NewRequest("GET", "http://192.168.1.20:8089/stream/run/"+w.ID()+"?after=999999.0", nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body := rec.Body.String()
		if !strings.Contains(body, "event: problem") {
			t.Fatalf("%s: no problem event in %q", remote, body)
		}
		if remote != "127.0.0.1:5000" && strings.Contains(body, filepath.Base(home)) {
			t.Errorf("the phone was told the path: %q", body)
		}
	}
}

func TestALimitedRunIsAWarningWithItsResetTime(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := runlog.Create("codex", agent.Task{Prompt: "fix it", Dir: "/repo"})
	if err != nil {
		t.Fatal(err)
	}
	resets := time.Date(2026, 10, 16, 15, 29, 0, 0, time.Local)
	if err := w.Close(agent.Result{Backend: "codex", Status: agent.StatusLimited, Error: "You've hit your usage limit.", LimitResets: resets}); err != nil {
		t.Fatal(err)
	}
	res := batch.Result{ID: "20261011-120000-abcdef", Started: time.Now().Add(-time.Minute), Dir: "/repo", Base: "0123456789abcdef", Parallel: 1,
		Duration: time.Minute,
		Tasks: []batch.TaskResult{{ID: "t1", Prompt: "fix it", Status: batch.StatusPass, Branch: "hrn/t1", Attempts: []batch.Attempt{
			{Backend: "codex", RunID: w.ID(), Result: agent.Result{Status: agent.StatusLimited}},
			{Backend: "claude", RunID: "20261011-120001-111111", Result: agent.Result{Status: agent.StatusOK}, Continued: true, SwitchedFrom: "codex"},
		}}}}
	saveBatch(t, res)
	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()

	_, body := get(t, srv.URL+"/run/"+w.ID())
	if !strings.Contains(body, `badge warn">limited<`) {
		t.Error("a limited run should carry the warning badge, not the failure one")
	}
	if !strings.Contains(body, "Limit lifts") || !strings.Contains(body, "2026-10-16 15:29") {
		t.Error("the run page does not say when the limit lifts")
	}
	_, body = get(t, srv.URL+"/batch/"+res.ID)
	if !strings.Contains(body, "moved from codex at its usage limit") {
		t.Error("the batch page does not say the attempt was moved from codex")
	}
}

func TestTheRepositoryOfASlotIsTheFolderAboveHrn(t *testing.T) {
	for dir, want := range map[string]string{
		"/home/me/myrepo/.hrn/slots/1": "myrepo",
		`C:\src\myrepo\.hrn\slots\12`:  "myrepo",
		"/home/me/myrepo/.hrn/other/1": "1",
	} {
		if got := repoOf(dir); got != want {
			t.Errorf("repoOf(%q) = %q, want %q", dir, got, want)
		}
	}
}

func TestTheRepositoryIsTheFolderAboveAWorktreeOrThePathsLastElement(t *testing.T) {
	for dir, want := range map[string]string{
		"/home/me/myrepo/.hrn/worktrees/t9":    "myrepo",
		`C:\src\myrepo\.hrn\worktrees\82-task`: "myrepo",
		"/home/me/myrepo":                      "myrepo",
		"/home/me/myrepo/":                     "myrepo",
		`C:\src\myrepo`:                        "myrepo",
		`C:\src\myrepo\`:                       "myrepo",
		`E:/mixed\style/repo`:                  "repo",
		"myrepo":                               "myrepo",
		"":                                     "",
	} {
		if got := repoOf(dir); got != want {
			t.Errorf("repoOf(%q) = %q, want %q", dir, got, want)
		}
	}
}

// batchRowOf is the batches table's row of one batch: from its link to the
// end of the row. A run's link to its batch puts class before href, so it
// does not match.
func batchRowOf(t *testing.T, body, id string) string {
	t.Helper()
	i := strings.Index(body, `<a href="/batch/`+id+`"`)
	if i < 0 {
		t.Fatalf("the index has no row for batch %s", id)
	}
	row, _, _ := strings.Cut(body[i:], "</tr>")
	return row
}

func TestTheIndexLeadsEachRunWithItsRepositoryAndTask(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	newRun := func(dir, prompt string) string {
		t.Helper()
		w, err := runlog.Create("fake", agent.Task{Dir: dir, Prompt: prompt})
		if err != nil {
			t.Fatal(err)
		}
		_ = w.Close(agent.Result{Backend: "fake", Status: agent.StatusOK})
		return w.ID()
	}
	// Four tasks of one batch open with the same paragraph; the last has an
	// id hrn made up.
	const opening = "Shared opening paragraph for every task.\n\nThen, for this one: "
	const wt = "/home/me/myrepo/.hrn/worktrees/"
	ids := []string{"82-repo-and-task", "83-purpose-card", "84-phone-layout", "t1"}
	runs := map[string]string{}
	var tasks []batch.TaskResult
	for _, id := range ids {
		runs[id] = newRun(wt+id, opening+id)
		tasks = append(tasks, batch.TaskResult{ID: id, Prompt: opening + id, Status: batch.StatusPass,
			Attempts: []batch.Attempt{{Backend: "fake", RunID: runs[id], Result: agent.Result{Status: agent.StatusOK}}}})
	}
	saveBatch(t, batch.Result{ID: "20261012-150000-abcdef", Started: time.Now(), Dir: "/home/me/myrepo", Duration: time.Minute, Tasks: tasks})
	plain, backslash, noDir := newRun("/src/harness", "a run on its own "+hostile), newRun(`C:\src\tidedesk\`, "on Windows"), newRun("", "no workspace")
	marked := newRun("/src/<b>bold<i>", "a folder named with markup")

	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	code, body := get(t, srv.URL+"/")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	check := func(where, html string) {
		t.Helper()
		labels := map[string]string{"82-repo-and-task": "myrepo · 82-repo-and-task", "83-purpose-card": "myrepo · 83-purpose-card",
			"84-phone-layout": "myrepo · 84-phone-layout", "t1": "myrepo · shared opening paragraph for every task"}
		for id, label := range labels {
			row := runRowOf(t, html, runs[id])
			if !strings.Contains(row, `">`+label+`</a>`) {
				t.Errorf("%s: task %s's run does not lead with %q:\n%s", where, id, label, row)
			}
			if !strings.Contains(row, `<span class="row-id">`+runs[id]+`</span>`) {
				t.Errorf("%s: task %s's run does not keep its id in the row:\n%s", where, id, row)
			}
		}
		for run, label := range map[string]string{plain: "harness", backslash: "tidedesk"} {
			row := runRowOf(t, html, run)
			if !strings.Contains(row, `">`+label+`</a><span class="row-id">`+run+`</span>`) {
				t.Errorf("%s: a run no batch started does not lead with its repository %q alone:\n%s", where, label, row)
			}
		}
		if row := runRowOf(t, html, noDir); !strings.Contains(row, `<a href="/run/`+noDir+`" class="mono">`+noDir+`</a>`) || strings.Contains(row, "row-id") {
			t.Errorf("%s: run %s without a workspace does not keep its id as its label:\n%s", where, noDir, row)
		}
		if row := runRowOf(t, html, marked); !strings.Contains(row, `">&lt;b&gt;bold&lt;i&gt;</a>`) {
			t.Errorf("%s: a repository's name is not escaped:\n%s", where, row)
		}
	}
	check("the index", body)
	assertEscaped(t, "the index", body)

	s, stop := openStream(t, srv.URL+"/stream/index", "")
	defer stop()
	m, ok := s.next(t)
	if !ok || m.event != "replace" {
		t.Fatalf("first message = %+v, %v", m, ok)
	}
	check("the streamed index", m.data)
}

func TestTheBatchesTableLeadsWithTheRepositoryAndNamesTheTasks(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	const five, three, noDir = "20261012-160000-aaaaaa", "20261012-150000-bbbbbb", "20261012-140000-cccccc"
	tasksNamed := func(ids ...string) []batch.TaskResult {
		var ts []batch.TaskResult
		for _, id := range ids {
			ts = append(ts, batch.TaskResult{ID: id, Prompt: "Shared opening.\nMore", Status: batch.StatusPass})
		}
		return ts
	}
	start := time.Now()
	saveBatch(t, batch.Result{ID: five, Started: start, Dir: `C:\src\tidedesk`, Duration: time.Minute, Tasks: tasksNamed("91-login", "92-logout", "t3", "94-billing", "95-export")})
	saveBatch(t, batch.Result{ID: three, Started: start.Add(-time.Hour), Dir: "/home/me/harness", Duration: time.Minute, Tasks: tasksNamed("a", "b", "c")})
	saveBatch(t, batch.Result{ID: noDir, Started: start.Add(-2 * time.Hour), Duration: time.Minute, Tasks: tasksNamed("only")})

	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	code, body := get(t, srv.URL+"/")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	check := func(where, html string) {
		t.Helper()
		if !strings.Contains(html, `<th scope="col">Tasks</th>`) || strings.Contains(html, "First task") {
			t.Errorf("%s: the batches table's last column is not Tasks", where)
		}
		row := batchRowOf(t, html, five)
		for _, want := range []string{`">tidedesk</a><span class="row-id">` + five + `</span>`, "91-login, 92-logout, shared opening and 2 more"} {
			if !strings.Contains(row, want) {
				t.Errorf("%s: the batch of five lacks %q:\n%s", where, want, row)
			}
		}
		if row := batchRowOf(t, html, three); !strings.Contains(row, `">harness</a>`) || !strings.Contains(row, "a, b, c<") || strings.Contains(row, "more") {
			t.Errorf("%s: the batch of three does not list all three tasks under its repository:\n%s", where, row)
		}
		if row := batchRowOf(t, html, noDir); !strings.Contains(row, `<a href="/batch/`+noDir+`" class="mono">`+noDir+`</a>`) || strings.Contains(row, "row-id") {
			t.Errorf("%s: a batch without a repository does not keep its id as its label:\n%s", where, row)
		}
	}
	check("the index", body)

	s, stop := openStream(t, srv.URL+"/stream/index", "")
	defer stop()
	m, ok := s.next(t)
	if !ok || m.event != "replace" {
		t.Fatalf("first message = %+v, %v", m, ok)
	}
	check("the streamed index", m.data)
}

// A task whose id hrn made up is named from its prompt's start, which topic
// may lowercase: the name is taken from the batch as blanked of secrets, or
// a key at the start of the prompt would reach the run's label changed past
// what the page's last pass looks for.
func TestARunsTaskNameIsBlankedOfSecrets(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	const secret = "Abcdefghijklmnop"
	t.Setenv("SERVICE_TOKEN", secret)
	prompt := secret + " starts this prompt"
	w, err := runlog.Create("fake", agent.Task{Dir: "/home/me/myrepo/.hrn/worktrees/t1", Prompt: prompt})
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Close(agent.Result{Backend: "fake", Status: agent.StatusOK})
	// The batch is past the 20 the index lists: its task still names the run.
	const id = "20261012-170000-abcdef"
	start := time.Now()
	saveBatch(t, batch.Result{ID: id, Started: start.Add(-time.Hour), Dir: "/home/me/myrepo", Duration: time.Minute,
		Tasks: []batch.TaskResult{{ID: "t1", Prompt: prompt, Status: batch.StatusPass,
			Attempts: []batch.Attempt{{Backend: "fake", RunID: w.ID(), Result: agent.Result{Status: agent.StatusOK}}}}}})
	for i := range 20 {
		saveBatch(t, batch.Result{ID: fmt.Sprintf("20261012-180000-%06x", i), Started: start.Add(-time.Duration(i+1) * time.Minute)})
	}

	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	check := func(where, html string) {
		t.Helper()
		if strings.Contains(html, `href="/batch/`+id+`">myrepo</a>`) {
			t.Fatalf("%s: the batch is in the batches table; the test needs it past the 20 listed", where)
		}
		if strings.Contains(strings.ToLower(html), strings.ToLower(secret)) {
			t.Errorf("%s shows the secret value, in some case:\n%s", where, html)
		}
		if row := runRowOf(t, html, w.ID()); !strings.Contains(row, `">myrepo · `) || !strings.Contains(row, `<span class="row-id">`+w.ID()+`</span>`) {
			t.Errorf("%s: the run's row has no label:\n%s", where, row)
		}
	}
	_, body := get(t, srv.URL+"/")
	check("the index", body)

	s, stop := openStream(t, srv.URL+"/stream/index", "")
	defer stop()
	m, ok := s.next(t)
	if !ok || m.event != "replace" {
		t.Fatalf("first message = %+v, %v", m, ok)
	}
	check("the streamed index", m.data)
}

// A workspace folder named like its run still names a repository: the row
// keeps the run's id under the label.
func TestARowKeepsItsIDWhenTheRepositoryIsNamedLikeIt(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := runlog.Create("fake", agent.Task{Prompt: "a run in a folder named like it"})
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Close(agent.Result{Backend: "fake", Status: agent.StatusOK})
	path := filepath.Join(runlog.Root(), w.ID(), "result.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec runlog.Record
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	rec.Task.Dir = "/src/" + w.ID()
	data, _ = json.Marshal(rec)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	_, body := get(t, srv.URL+"/")
	if row := runRowOf(t, body, w.ID()); !strings.Contains(row, `">`+w.ID()+`</a><span class="row-id">`+w.ID()+`</span>`) {
		t.Errorf("the run's row does not keep its id under the label:\n%s", row)
	}
}

// A name made from a prompt's start is the prompt's text: markup in it is
// shown, not run, on the index and in its stream.
func TestAGeneratedTaskNameIsEscaped(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	prompt := "<b>x</b> in the task's first words"
	w, err := runlog.Create("fake", agent.Task{Dir: "/home/me/myrepo/.hrn/worktrees/t1", Prompt: prompt})
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Close(agent.Result{Backend: "fake", Status: agent.StatusOK})
	saveBatch(t, batch.Result{ID: "20261012-180000-abcdef", Started: time.Now(), Dir: "/home/me/myrepo", Duration: time.Minute,
		Tasks: []batch.TaskResult{{ID: "t1", Prompt: prompt, Status: batch.StatusPass,
			Attempts: []batch.Attempt{{Backend: "fake", RunID: w.ID(), Result: agent.Result{Status: agent.StatusOK}}}}}})

	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	check := func(where, html string) {
		t.Helper()
		if strings.Contains(html, "<b>") {
			t.Errorf("%s holds the task name's markup unescaped", where)
		}
		if row := runRowOf(t, html, w.ID()); !strings.Contains(row, `">myrepo · &lt;b&gt;x&lt;/b&gt; in the task&#39;s first words</a>`) {
			t.Errorf("%s: the run's label is not the escaped task name:\n%s", where, row)
		}
		if row := batchRowOf(t, html, "20261012-180000-abcdef"); !strings.Contains(row, "&lt;b&gt;x&lt;/b&gt;") {
			t.Errorf("%s: the batch's task name is not escaped:\n%s", where, row)
		}
	}
	_, body := get(t, srv.URL+"/")
	check("the index", body)

	s, stop := openStream(t, srv.URL+"/stream/index", "")
	defer stop()
	m, ok := s.next(t)
	if !ok || m.event != "replace" {
		t.Fatalf("first message = %+v, %v", m, ok)
	}
	check("the streamed index", m.data)
}

// The batches table's headers stand over their cells: Passed over the pass
// count, Tasks over the names.
func TestTheBatchesTablesHeadersLineUpWithItsCells(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	const id = "20261012-190000-abcdef"
	saveBatch(t, batch.Result{ID: id, Started: time.Now(), Dir: "/home/me/myrepo", Duration: time.Minute,
		Tasks: []batch.TaskResult{{ID: "91-login", Status: batch.StatusPass}, {ID: "92-logout", Status: batch.StatusFail}}})
	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	_, body := get(t, srv.URL+"/")
	_, table, _ := strings.Cut(body, `aria-label="Batches"`)
	table, _, _ = strings.Cut(table, "</table>")
	head, _, _ := strings.Cut(table, "</thead>")
	var headers []string
	for _, th := range strings.Split(head, "<th ")[1:] {
		_, text, _ := strings.Cut(th, ">")
		text, _, _ = strings.Cut(text, "</th>")
		headers = append(headers, text)
	}
	cells := strings.Split(batchRowOf(t, body, id), "<td")
	cells = cells[1:] // the row starts inside its first cell
	if len(cells) != len(headers)-1 {
		t.Fatalf("%d headers over %d cells after the first:\n%v\n%s", len(headers), len(cells), headers, table)
	}
	for header, want := range map[string]string{"Passed": "1/2 passed", "Tasks": "91-login, 92-logout"} {
		i := slices.Index(headers, header)
		if i < 1 || !strings.Contains(cells[i-1], want) {
			t.Errorf("header %q (column %d) is not over %q: %v\n%s", header, i, want, headers, table)
		}
	}
}

// viewAs fetches path from h as a browser at remote sees it.
func viewAs(t *testing.T, h http.Handler, path, remote string) string {
	t.Helper()
	host := "127.0.0.1:8089"
	if !strings.HasPrefix(remote, "127.") {
		host = "192.168.1.20:8089"
	}
	req := httptest.NewRequest("GET", "http://"+host+path, nil)
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("%s from %s: status %d: %s", path, remote, rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// jsonPath is p as a JSON string holds it, without the quotes.
func jsonPath(p string) string {
	b, _ := json.Marshal(p)
	return strings.Trim(string(b), `"`)
}

func TestTheRunPageNamesItsWorkspaceAndShortensItsPaths(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	ws := filepath.Join(t.TempDir(), "ada-os-ipc")
	readme := filepath.Join(ws, "README.md")
	short := "." + string(filepath.Separator) + "README.md"
	w, err := runlog.Create("fake", agent.Task{Prompt: "fix " + readme, Dir: ws})
	if err != nil {
		t.Fatal(err)
	}
	w.Event(agent.Event{Kind: agent.EventInfo, Text: "codex exec -C " + ws + " --json"})
	w.Event(agent.Event{Kind: agent.EventToolCall, Tool: "Read", Input: json.RawMessage(`{"file_path":"` + jsonPath(readme) + `"}`)})
	w.Event(agent.Event{Kind: agent.EventToolResult, Text: "read " + readme})
	if err := w.Close(agent.Result{Backend: "fake", Status: agent.StatusOK, Final: "changed " + readme}); err != nil {
		t.Fatal(err)
	}
	h := New(context.Background(), Options{LAN: []Endpoint{{Addr: "192.168.1.20:8089"}}})

	phone := viewAs(t, h, "/run/"+w.ID(), "192.168.1.50:6000")
	for _, bad := range []string{ws, jsonPath(ws), "title="} {
		if strings.Contains(phone, bad) {
			t.Errorf("the phone's run page holds %q", bad)
		}
	}
	for _, want := range []string{"<dd>ada-os-ipc</dd>", "codex exec -C . --json", "fix " + short, "read " + short, "changed " + short} {
		if !strings.Contains(phone, want) {
			t.Errorf("the phone's run page lacks %q", want)
		}
	}

	local := viewAs(t, h, "/run/"+w.ID(), "127.0.0.1:5000")
	if !strings.Contains(local, `<dd title="`+ws+`">ada-os-ipc</dd>`) || strings.Count(local, ws) != 1 {
		t.Errorf("this computer should see the full workspace once, as the name's title; it is there %d times", strings.Count(local, ws))
	}
}

func TestTheUsageChartShowsShortenedPathsOnTheRunPageAndInItsStream(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	ws := filepath.Join(t.TempDir(), "ada-os-ipc")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	homeNote := filepath.Join(home, "notes.txt")
	w, err := runlog.Create("claude", agent.Task{Prompt: "p", Dir: ws})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close(agent.Result{Status: agent.StatusOK}) })
	w.Event(agent.Event{Kind: agent.EventUsage, Usage: &agent.Usage{Model: "m", ContextTokens: 10000}})
	w.Event(agent.Event{Kind: agent.EventCompaction, Text: "before " + ws + " near " + homeNote, Usage: &agent.Usage{ContextTokens: 10000}})
	w.Event(agent.Event{Kind: agent.EventUsage, Usage: &agent.Usage{Model: "m", ContextTokens: 2000}})
	h := New(context.Background(), Options{Poll: 5 * time.Millisecond, LAN: []Endpoint{{Addr: "192.168.1.20:8089"}}})
	page := viewAs(t, h, "/run/"+w.ID(), "192.168.1.50:6000")
	chart := chartOf(t, page)
	want := "<title>compacted before turn 2 (before . near ~" + string(filepath.Separator) + "notes.txt)"
	if !strings.Contains(chart, want) {
		t.Errorf("the page chart lacks %q:\n%s", want, chart)
	}
	if strings.Contains(chart, ws) || strings.Contains(chart, homeNote) {
		t.Errorf("the page chart holds a full path:\n%s", chart)
	}

	i := strings.Index(page, `data-stream="`)
	if i < 0 {
		t.Fatal("a running run's page has no stream")
	}
	streamPath := page[i+len(`data-stream="`):]
	streamPath = strings.ReplaceAll(streamPath[:strings.Index(streamPath, `"`)], "&amp;", "&")
	srv := httptest.NewServer(h)
	defer srv.Close()
	st, stop := openStream(t, srv.URL+streamPath, "")
	defer stop()
	w.Event(agent.Event{Kind: agent.EventCompaction, Text: "after " + ws, Usage: &agent.Usage{ContextTokens: 12000}})
	w.Event(agent.Event{Kind: agent.EventUsage, Usage: &agent.Usage{Model: "m", ContextTokens: 3000}})
	var m message
	found := false
	for range 4 {
		var ok bool
		m, ok = st.next(t)
		if !ok || m.event == "end" {
			t.Fatalf("stream ended before the later compaction appeared: %+v", m)
		}
		if m.event == "usage" && strings.Contains(m.data, "3 turns") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("the stream did not redraw the chart over three turns: %+v", m)
	}
	for _, want := range []string{"<title>compacted before turn 2 (before . near ~" + string(filepath.Separator) + "notes.txt)", "<title>compacted before turn 3 (after .)"} {
		if !strings.Contains(m.data, want) {
			t.Errorf("the streamed chart lacks %q:\n%s", want, m.data)
		}
	}
	if strings.Contains(m.data, ws) || strings.Contains(m.data, homeNote) {
		t.Errorf("the streamed chart holds a full path:\n%s", m.data)
	}
}

func TestTheRunStreamShortensItsPaths(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	ws := filepath.Join(t.TempDir(), "ada-os-ipc")
	w, err := runlog.Create("fake", agent.Task{Prompt: "p", Dir: ws})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	s, stop := openStream(t, srv.URL+"/stream/run/"+w.ID(), "")
	defer stop()
	w.Event(agent.Event{Kind: agent.EventText, Text: "working in " + ws + " now"})
	_ = w.Close(agent.Result{Status: agent.StatusOK, Final: "done in " + ws})
	var all string
	for {
		m, ok := s.next(t)
		if !ok || m.event == "end" {
			break
		}
		all += m.data
	}
	if strings.Contains(all, ws) || !strings.Contains(all, "working in . now") || !strings.Contains(all, "done in .") {
		t.Errorf("the stream does not shorten the workspace:\n%s", all)
	}
}

func TestTheBatchPageNamesItsRepositoryAndShortensItsPaths(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	repo := filepath.Join(t.TempDir(), "ada-os-ipc")
	wt := filepath.Join(repo, ".hrn", "worktrees", "t1")
	sep := string(filepath.Separator)
	res := batch.Result{ID: "20261010-120000-abcdef", Started: time.Now().Add(-time.Minute), Duration: time.Minute, Dir: repo, Parallel: 1,
		Tasks: []batch.TaskResult{{ID: "t1", Prompt: "edit " + filepath.Join(wt, "a.go"), Status: batch.StatusFail, Branch: "hrn/t1", Worktree: wt,
			Attempts: []batch.Attempt{{Backend: "codex", RunID: "20261010-120001-111111", Result: agent.Result{Status: agent.StatusOK},
				Gates: []batch.GateResult{{Command: "go vet " + wt + "/...", ExitCode: 1, Output: filepath.Join(wt, "a.go") + ":3: bad"}}}}}}}
	saveBatch(t, res)
	h := New(context.Background(), Options{LAN: []Endpoint{{Addr: "192.168.1.20:8089"}}})

	phone := viewAs(t, h, "/batch/"+res.ID, "192.168.1.50:6000")
	for _, bad := range []string{repo, "title="} {
		if strings.Contains(phone, bad) {
			t.Errorf("the phone's batch page holds %q", bad)
		}
	}
	for _, want := range []string{"<dd>ada-os-ipc</dd>", "<dd>." + sep + filepath.Join(".hrn", "worktrees", "t1") + "</dd>",
		"edit ." + sep + "a.go", "go vet ./...", "." + sep + "a.go:3: bad"} {
		if !strings.Contains(phone, want) {
			t.Errorf("the phone's batch page lacks %q", want)
		}
	}

	local := viewAs(t, h, "/batch/"+res.ID, "127.0.0.1:5000")
	for _, want := range []string{`<dd title="` + repo + `">ada-os-ipc</dd>`, `<dd title="` + wt + `">.`} {
		if !strings.Contains(local, want) {
			t.Errorf("this computer's batch page lacks %q", want)
		}
	}
	if n := strings.Count(local, repo); n != 2 {
		t.Errorf("this computer should see the full paths only as the two titles; they are there %d times", n)
	}
}

func TestTheIndexShortensThePathsInItsRowsForEveryViewer(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	const key = "sk-ant-api03-test-0123456789abcdefXYZ"
	t.Setenv("ANTHROPIC_API_KEY", key)
	sep := string(filepath.Separator)
	ws := filepath.Join(t.TempDir(), "ada-os-ipc")
	w, err := runlog.Create("fake", agent.Task{Prompt: "fix " + filepath.Join(ws, "README.md") + " with " + filepath.Join(ws, key), Dir: ws})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{Backend: "fake", Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(t.TempDir(), "other-repo")
	wt := filepath.Join(repo, ".hrn", "worktrees", "t1")
	saveBatch(t, batch.Result{ID: "20261010-120000-abcdef", Started: time.Now().Add(-time.Minute), Duration: time.Minute, Dir: repo, Parallel: 1,
		Tasks: []batch.TaskResult{{ID: "t1", Prompt: "edit " + filepath.Join(wt, "a.go"), Status: batch.StatusPass, Branch: "hrn/t1", Worktree: wt,
			Attempts: []batch.Attempt{{Backend: "codex", RunID: "20261010-120001-111111", Result: agent.Result{Status: agent.StatusOK}}}}}})
	h := New(context.Background(), Options{Poll: 5 * time.Millisecond, LAN: []Endpoint{{Addr: "192.168.1.20:8089"}}})

	// The stream runs until its request is done; a short deadline ends it.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest("GET", "http://192.168.1.20:8089/stream/index", nil).WithContext(ctx)
	req.RemoteAddr = "192.168.1.50:6000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	for name, body := range map[string]string{"page": viewAs(t, h, "/", "192.168.1.50:6000"), "stream": rec.Body.String()} {
		for _, bad := range []string{ws, repo, key} {
			if strings.Contains(body, bad) {
				t.Errorf("the phone's index %s holds %q", name, bad)
			}
		}
		for _, want := range []string{"fix ." + sep + "README.md with ." + sep + "[redacted]", "edit ." + sep + "a.go"} {
			if !strings.Contains(body, want) {
				t.Errorf("the phone's index %s lacks %q", name, want)
			}
		}
	}
}

func TestTheIndexClassifiesThePromptAsRecordedNotAsShortened(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	ws := filepath.Join(t.TempDir(), "project", "ci")
	w, err := runlog.Create("fake", agent.Task{Prompt: "Inspect " + filepath.Join(ws, "workflow.yml"), Dir: ws})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{Backend: "fake", Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	h := New(context.Background(), Options{Poll: 5 * time.Millisecond})

	page := viewAs(t, h, "/", "127.0.0.1:6000")
	row := runRowOf(t, page, w.ID())
	if !strings.Contains(row, `title="ops.ci-container">ops</td>`) {
		t.Errorf("the run's row is not ops, as hrn report has it:\n%s", row)
	}
	if !strings.Contains(page, `<th scope="row">ops</th>`) {
		t.Error("the purpose card has no ops row")
	}
	if want := "Inspect ." + string(filepath.Separator) + "workflow.yml"; !strings.Contains(row, want) {
		t.Errorf("the row's prompt lacks %q:\n%s", want, row)
	}
	if strings.Contains(row, ws) {
		t.Errorf("the row's prompt holds the full path %q", ws)
	}
}

func TestTheBatchStreamShortensItsPaths(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	repo := filepath.Join(t.TempDir(), "ada-os-ipc")
	wt := filepath.Join(repo, ".hrn", "worktrees", "t1")
	sep := string(filepath.Separator)
	res := batch.Result{ID: "20261010-120000-abcdef", Started: time.Now().Add(-time.Minute), Duration: time.Minute, Dir: repo, Parallel: 1,
		Tasks: []batch.TaskResult{{ID: "t1", Prompt: "edit " + filepath.Join(wt, "a.go"), Status: batch.StatusFail, Branch: "hrn/t1", Worktree: wt,
			Attempts: []batch.Attempt{{Backend: "codex", RunID: "20261010-120001-111111", Result: agent.Result{Status: agent.StatusOK},
				Gates: []batch.GateResult{{Command: "go vet " + wt + "/...", ExitCode: 1, Output: filepath.Join(wt, "a.go") + ":3: bad"}}}}}}}
	saveBatch(t, res)
	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	s, stop := openStream(t, srv.URL+"/stream/batch/"+res.ID, "")
	defer stop()
	var all string
	for {
		m, ok := s.next(t)
		if !ok || m.event == "end" {
			break
		}
		all += m.data
	}
	for _, want := range []string{"edit ." + sep + "a.go", "go vet ./...", "." + sep + "a.go:3: bad"} {
		if !strings.Contains(all, want) {
			t.Errorf("the batch stream lacks %q", want)
		}
	}
	// This computer sees the whole worktree only as its fact's hover title.
	if n := strings.Count(all, wt); n != 1 || !strings.Contains(all, `title="`+wt+`"`) {
		t.Errorf("the batch stream holds the worktree's path %d times, want once as a title:\n%s", n, all)
	}
}

func TestMaskDisplayKeepsRecordedSecrets(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	const token = "sk-test-display-only-0123456789"
	const prompt = "API_KEY=fake-prompt-0123456789 do the task"
	w, err := runlog.Create("fake", agent.Task{Prompt: prompt})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(agent.Result{Status: agent.StatusOK})
	command := "curl -H \"Authorization: Bearer " + token + "\""
	w.Event(agent.Event{Kind: agent.EventToolCall, Tool: "Bash", Text: command, Input: json.RawMessage(`{"command":` + strconv.Quote(command) + `}`)})
	h := New(context.Background(), Options{Poll: time.Millisecond, LAN: []Endpoint{{Addr: "192.168.1.20:8089"}}})
	time.Sleep(settle) // A newly created run is deliberately held back by the bot.
	for _, remote := range []string{"127.0.0.1:6000", "192.168.1.50:6000"} {
		for _, path := range []string{"/", "/run/" + w.ID(), "/bot", "/stream/index", "/stream/run/" + w.ID(), "/stream/bot"} {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			host := "127.0.0.1:8089"
			if strings.HasPrefix(remote, "192.") {
				host = "192.168.1.20:8089"
			}
			req := httptest.NewRequest("GET", "http://"+host+path, nil).WithContext(ctx)
			req.RemoteAddr = remote
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			cancel()
			body := rec.Body.String()
			if rec.Code != 200 || strings.Contains(body, token) || strings.Contains(body, "fake-prompt-0123456789") || !strings.Contains(body, "••••") {
				t.Errorf("%s %s status=%d lacks masking", remote, path, rec.Code)
			}
		}
	}
	rec, err := runlog.Load(w.ID())
	if err != nil || rec.Task.Prompt != prompt {
		t.Fatalf("record changed: %+v %v", rec, err)
	}
	events, err := runlog.Events(w.ID())
	if err != nil || len(events) != 1 || events[0].Text != command {
		t.Fatalf("events changed: %+v %v", events, err)
	}
}

func TestBotMasksBeforeMakingWords(t *testing.T) {
	for _, got := range []string{
		activity("Read", "sk-test-private"), commandWords("sk-test-private"),
		topic("API_KEY=private do something"),
		announce(botSnapshot{}, botSnapshot{items: []botItem{{ID: "r", Kind: "run", Who: "agent", Going: true, What: "API_KEY=private"}}}, false)[0].Text,
	} {
		if strings.Contains(got, "private") || !strings.Contains(got, "••••") {
			t.Errorf("bot leaks: %q", got)
		}
	}
}

func TestQuotaEventsStayOffTheRunTimeline(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := runlog.Create("claude", agent.Task{Prompt: "p", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	quota := agent.Event{Kind: agent.EventQuota, Quota: &agent.Quota{Week: &agent.QuotaWindow{UsedPercent: 20, ResetsAt: 1793000000}}}
	w.Event(agent.Event{Kind: agent.EventText, Text: "first"})
	w.Event(quota)
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	page := viewAs(t, New(context.Background(), Options{}), "/run/"+w.ID(), "127.0.0.1:6000")
	if !strings.Contains(page, "first") || strings.Contains(page, `<span class="tag">quota</span>`) || strings.Contains(page, ">quota<") {
		t.Fatalf("the timeline lists the quota event:\n%s", page)
	}
}
