package watch

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestServeRefusesAListenerThatIsNotLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Skip("cannot listen on every interface here:", err)
	}
	defer ln.Close()
	if err := Serve(context.Background(), Options{}, ln); err == nil || !strings.Contains(err.Error(), "loopback") {
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
		}}
	saveBatch(t, res)
	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()

	code, body := get(t, srv.URL+"/batch/"+res.ID)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{`href="/run/20261010-120001-111111"`, `href="/run/20261010-120002-222222"`,
		"go vet ./...", "go test ./...", ">1<", "<details", "first line", "running", `data-stream="/stream/batch/` + res.ID} {
		if !strings.Contains(body, want) {
			t.Errorf("the batch page lacks %q", want)
		}
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
	for _, want := range []string{"1m30s", "$0.0123", "~$0.7500", "codex", "running", "20261010-130000-cccccc", "batch task (and 1 more)", `data-stream="/stream/index"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the index lacks %q", want)
		}
	}
	assertEscaped(t, "the index", body)
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
