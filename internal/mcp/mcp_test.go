package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func echoServer() *Server {
	s := NewServer(Implementation{Name: "test", Version: "1"}, "be nice")
	s.AddTool(Tool{
		Name:        "echo",
		Description: "Echo text back.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []any{"text"}},
		Annotations: &Annotations{ReadOnlyHint: Bool(true)},
		Handler: func(ctx context.Context, call Call) (CallResult, error) {
			var a struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(call.Arguments, &a); err != nil {
				return CallResult{}, err
			}
			call.Progress("echoing")
			return CallResult{Content: []Content{TextContent("echo: " + a.Text)}, StructuredContent: map[string]any{"text": a.Text}}, nil
		},
	})
	s.AddTool(Tool{Name: "fail", Handler: func(context.Context, Call) (CallResult, error) { return CallResult{}, errors.New("boom") }})
	return s
}

// pipeClient serves srv over in-memory pipes and connects cl to it.
func pipeClient(t *testing.T, srv *Server, cl *Client) *Client {
	t.Helper()
	toServer, clientOut := io.Pipe()
	toClient, serverOut := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, toServer, serverOut) }()
	t.Cleanup(func() {
		cancel()
		clientOut.Close()
		serverOut.Close()
		<-done
	})
	if cl == nil {
		cl = &Client{}
	}
	if err := cl.Connect(ctx, toClient, clientOut); err != nil {
		t.Fatal(err)
	}
	return cl
}

func TestPipeRoundTrip(t *testing.T) {
	var mu sync.Mutex
	var progress []string
	cl := pipeClient(t, echoServer(), &Client{Progress: func(m string) { mu.Lock(); progress = append(progress, m); mu.Unlock() }})
	ctx := context.Background()

	if cl.ServerInfo().Name != "test" || cl.Instructions() != "be nice" {
		t.Fatalf("handshake: info=%+v instructions=%q", cl.ServerInfo(), cl.Instructions())
	}
	tools := cl.Tools()
	if len(tools) != 2 || tools[0].Name != "echo" || !tools[0].Annotations.ReadOnly() || tools[1].Annotations.ReadOnly() {
		t.Fatalf("tools = %+v", tools)
	}
	if !strings.Contains(string(tools[0].InputSchema), `"required":["text"]`) {
		t.Errorf("schema not passed through: %s", tools[0].InputSchema)
	}

	res, err := cl.CallTool(ctx, "echo", json.RawMessage(`{"text":"hi"}`))
	if err != nil || res.IsError || res.Text() != "echo: hi" {
		t.Fatalf("echo: res=%+v err=%v", res, err)
	}
	if sc, _ := res.StructuredContent.(map[string]any); sc["text"] != "hi" {
		t.Errorf("structured content lost: %v", res.StructuredContent)
	}
	mu.Lock()
	got := strings.Join(progress, ",")
	mu.Unlock()
	if got != "echoing" {
		t.Errorf("progress = %q, want echoing", got)
	}

	res, err = cl.CallTool(ctx, "fail", nil)
	if err != nil || !res.IsError || res.Text() != "boom" {
		t.Fatalf("handler error should become IsError: res=%+v err=%v", res, err)
	}

	_, err = cl.CallTool(ctx, "nope", nil)
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeInvalidParams {
		t.Fatalf("unknown tool: err=%v", err)
	}
	if err := cl.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCancelPropagatesToHandler(t *testing.T) {
	cancelled := make(chan struct{}, 1)
	srv := NewServer(Implementation{Name: "t", Version: "1"}, "")
	srv.AddTool(Tool{Name: "slow", Handler: func(ctx context.Context, _ Call) (CallResult, error) {
		<-ctx.Done()
		cancelled <- struct{}{}
		return CallResult{}, ctx.Err()
	}})
	cl := pipeClient(t, srv, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := cl.CallTool(ctx, "slow", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("server handler was not cancelled")
	}
	if err := cl.Ping(context.Background()); err != nil {
		t.Fatalf("connection unusable after cancel: %v", err)
	}
}

func TestServerSurvivesBadJSON(t *testing.T) {
	toServer, clientOut := io.Pipe()
	toClient, serverOut := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- echoServer().Serve(ctx, toServer, serverOut) }()
	t.Cleanup(func() { cancel(); clientOut.Close(); serverOut.Close(); <-done })

	c := newCodec(toClient, clientOut)
	go clientOut.Write([]byte("this is not json\n"))
	m, err := c.read()
	if err != nil || m.Error == nil || m.Error.Code != CodeParseError || string(m.ID) != "null" {
		t.Fatalf("parse error response: m=%+v err=%v", m, err)
	}
	go c.request(json.RawMessage("1"), "ping", nil)
	m, err = c.read()
	if err != nil || string(m.ID) != "1" || string(m.Result) != "{}" {
		t.Fatalf("ping after bad line: m=%+v err=%v", m, err)
	}
}

func TestClientAnswersServerRequests(t *testing.T) {
	toServer, clientOut := io.Pipe()
	toClient, serverOut := io.Pipe()
	srv := newCodec(toServer, serverOut)
	var logs []string
	root := t.TempDir()
	cl := &Client{Roots: []string{root}, Log: func(s string) { logs = append(logs, s) }}
	errc := make(chan error, 1)
	go func() { errc <- cl.Connect(context.Background(), toClient, clientOut) }()
	t.Cleanup(func() { clientOut.Close(); serverOut.Close() })

	read := func(method string) *message {
		t.Helper()
		m, err := srv.read()
		if err != nil {
			t.Fatal(err)
		}
		if method != "" && m.Method != method {
			t.Fatalf("got %s, want %s", m.Method, method)
		}
		return m
	}
	m := read("initialize")
	var p initializeParams
	_ = json.Unmarshal(m.Params, &p)
	if p.ProtocolVersion != ProtocolVersion || p.ClientInfo.Name != "hx" {
		t.Errorf("initialize params = %+v", p)
	}
	_ = srv.respond(m.ID, initializeResult{ProtocolVersion: ProtocolVersion, ServerInfo: Implementation{Name: "fake", Version: "0"}, Capabilities: map[string]any{}})
	read("notifications/initialized")
	m = read("tools/list")
	_ = srv.respond(m.ID, listToolsResult{Tools: []ToolInfo{{Name: "a", InputSchema: json.RawMessage(`{}`)}}, NextCursor: "p2"})
	m = read("tools/list")
	var lp listToolsParams
	_ = json.Unmarshal(m.Params, &lp)
	if lp.Cursor != "p2" {
		t.Errorf("pagination cursor not sent: %+v", lp)
	}
	_ = srv.respond(m.ID, listToolsResult{Tools: []ToolInfo{{Name: "b", InputSchema: json.RawMessage(`{}`)}}})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if len(cl.Tools()) != 2 {
		t.Fatalf("tools = %+v", cl.Tools())
	}

	_ = srv.request(json.RawMessage(`"r1"`), "roots/list", nil)
	m = read("")
	if string(m.ID) != `"r1"` || !strings.Contains(string(m.Result), "file:///") || !strings.Contains(string(m.Result), filepath.Base(root)) {
		t.Errorf("roots/list response = %s", m.Result)
	}
	_ = srv.request(json.RawMessage("7"), "ping", nil)
	if m = read(""); string(m.Result) != "{}" {
		t.Errorf("ping response = %+v", m)
	}
	_ = srv.request(json.RawMessage("8"), "sampling/createMessage", nil)
	if m = read(""); m.Error == nil || m.Error.Code != CodeMethodNotFound {
		t.Errorf("unsupported request should be method-not-found: %+v", m)
	}
	_ = srv.notify("notifications/message", logParams{Level: "warning", Data: json.RawMessage(`"disk almost full"`)})
	_ = srv.notify("notifications/message", logParams{Level: "debug", Data: json.RawMessage(`"noise"`)})
	_ = srv.request(json.RawMessage("9"), "ping", nil)
	read("")
	if strings.Join(logs, "|") != "warning: disk almost full" {
		t.Errorf("logs = %v", logs)
	}
}

// TestHelperMCPServer is not a test: the stdio tests re-run the test binary
// with HX_MCP_HELPER=1 so it serves echoServer on its own stdin/stdout.
func TestHelperMCPServer(t *testing.T) {
	switch os.Getenv("HX_MCP_HELPER") {
	case "1":
		_ = echoServer().Serve(context.Background(), os.Stdin, os.Stdout)
		os.Exit(0)
	case "crash":
		fmt.Fprintln(os.Stderr, "helper crashing on purpose")
		os.Exit(3)
	}
}

func helperConfig(mode string) ServerConfig {
	return ServerConfig{Command: os.Args[0], Args: []string{"-test.run=^TestHelperMCPServer$", "--"}, Env: map[string]string{"HX_MCP_HELPER": mode}}
}

func TestStdioProcess(t *testing.T) {
	cl := &Client{}
	cfg := helperConfig("1")
	cfg.Dir = t.TempDir()
	if err := cl.Start(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	res, err := cl.CallTool(context.Background(), "echo", json.RawMessage(`{"text":"proc"}`))
	if err != nil || res.Text() != "echo: proc" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if err := cl.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := cl.CallTool(context.Background(), "echo", nil); err == nil {
		t.Fatal("call after close should fail")
	}
}

func TestStartFailureIncludesStderr(t *testing.T) {
	err := (&Client{}).Start(context.Background(), helperConfig("crash"))
	if err == nil || !strings.Contains(err.Error(), "helper crashing on purpose") {
		t.Fatalf("err = %v", err)
	}
	if err := (&Client{}).Start(context.Background(), ServerConfig{Command: "hx-definitely-missing-binary"}); err == nil {
		t.Fatal("missing binary should fail to start")
	}
}

func TestLoadConfig(t *testing.T) {
	t.Setenv("HX_TEST_TOKEN", "tok")
	t.Setenv("HX_TEST_EMPTY", "")
	p := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(p, []byte(`{"mcpServers":{
		"zeta":{"command":"z","args":["--token","${HX_TEST_TOKEN}","${HX_TEST_EMPTY:-dflt}","${HX_NOPE_UNSET}","literal $x"],"env":{"K":"${HX_TEST_TOKEN}"}},
		"alpha":{"command":"a"},
		"remote":{"type":"http","url":"https://x"},
		"sse":{"url":"https://y"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	servers, skipped, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 2 || servers[0].Name != "alpha" || servers[1].Name != "zeta" {
		t.Fatalf("servers = %+v", servers)
	}
	if got := strings.Join(servers[1].Args, "|"); got != "--token|tok|dflt||literal $x" {
		t.Errorf("args = %q", got)
	}
	if servers[1].Env["K"] != "tok" {
		t.Errorf("env = %v", servers[1].Env)
	}
	if strings.Join(skipped, ",") != "remote,sse" {
		t.Errorf("skipped = %v", skipped)
	}
}

func TestLoadConfigsLaterFileWins(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json")
	_ = os.WriteFile(a, []byte(`{"mcpServers":{"one":{"command":"first"},"two":{"command":"t"}}}`), 0o644)
	_ = os.WriteFile(b, []byte(`{"mcpServers":{"one":{"command":"second"}}}`), 0o644)
	servers, _, err := LoadConfigs([]string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 2 || servers[0].Name != "one" || servers[0].Command != "second" || servers[1].Name != "two" {
		t.Fatalf("servers = %+v", servers)
	}
}

func TestConfigPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HX_HOME", home)
	paths, err := ConfigPaths(nil)
	if err != nil || len(paths) != 0 {
		t.Fatalf("paths=%v err=%v", paths, err)
	}
	def := filepath.Join(home, "mcp.json")
	_ = os.WriteFile(def, []byte(`{}`), 0o644)
	extra := filepath.Join(t.TempDir(), "x.json")
	_ = os.WriteFile(extra, []byte(`{}`), 0o644)
	paths, err = ConfigPaths([]string{extra})
	if err != nil || len(paths) != 2 || paths[0] != def || paths[1] != extra {
		t.Fatalf("paths=%v err=%v", paths, err)
	}
	if _, err := ConfigPaths([]string{"missing.json"}); err == nil {
		t.Fatal("missing extra config should error")
	}
}

func TestCallResultText(t *testing.T) {
	r := CallResult{Content: []Content{
		TextContent("a"),
		{Type: "image", MimeType: "image/png", Data: "AAAA"},
		{Type: "resource", Resource: json.RawMessage(`{"uri":"file:///x","text":"inner"}`)},
		{Type: "resource_link", URI: "file:///y"},
	}}
	if got := r.Text(); got != "a\n[image image/png, 3 bytes]\ninner\n[resource file:///y]" {
		t.Errorf("Text() = %q", got)
	}
	r = CallResult{StructuredContent: map[string]any{"k": 1}}
	if got := r.Text(); got != `{"k":1}` {
		t.Errorf("structured fallback = %q", got)
	}
}
