package execproto

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWorkerRestrictions(t *testing.T) {
	for _, tc := range []struct{ name, backend, endpoint, extra string }{
		{"other backend", "codex", "http://localhost", ""},
		{"missing backend", "", "http://localhost", ""},
		{"remote host", "api", "http://example.com", ""},
		{"https", "api", "https://localhost", ""},
		{"userinfo", "api", "http://user@localhost", ""},
		{"missing endpoint", "api", "", ""},
		{"mcp requested", "api", "http://localhost", `,"mcp":["tools.json"]`},
		{"tools requested", "api", "http://localhost", `,"tools":["shell"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			t.Setenv("ANTHROPIC_BASE_URL", tc.endpoint)
			task, _ := json.Marshal(start(t.TempDir(), tc.backend))
			input := strings.TrimSuffix(string(task), "}}") + tc.extra + "}}\n"
			var out bytes.Buffer
			if code := RunWorker(context.Background(), strings.NewReader(input), &out, nil); code != 2 || !strings.Contains(out.String(), `"type":"rejected"`) {
				t.Fatalf("code=%d output=%s", code, out.String())
			}
		})
	}
}

func TestWorkerAcceptsLoopbackWithoutTools(t *testing.T) {
	requests := make(chan map[string]json.RawMessage, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		requests <- request
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"test\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"done\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("ANTHROPIC_BASE_URL", server.URL)
	t.Setenv("ANTHROPIC_API_KEY", "fixture")
	input, _ := json.Marshal(start(t.TempDir(), "api"))
	var out bytes.Buffer
	// Keep stdin open until the backend finishes.
	reader, writer := io.Pipe()
	go func() { fmt.Fprintln(writer, string(input)) }()
	defer writer.Close()
	if code := RunWorker(context.Background(), reader, &out, nil); code != 0 || !strings.Contains(out.String(), `"type":"accepted"`) {
		t.Fatalf("code=%d output=%s", code, out.String())
	}
	request := <-requests
	if tools, ok := request["tools"]; ok && string(tools) != "null" && string(tools) != "[]" {
		t.Fatalf("offered tools: %s", tools)
	}
}

func TestPlainExecStillAcceptsOtherBackendAndMCP(t *testing.T) {
	name := registerFake(t, nil)
	t.Setenv("ANTHROPIC_BASE_URL", "https://example.com")
	h := openHarness(t, nil)
	h.next(t, "hello")
	request := start(t.TempDir(), name)
	request["task"].(map[string]any)["mcp"] = []string{}
	h.send(t, request)
	h.next(t, "accepted")
	h.send(t, map[string]any{"type": "cancel", "attempt_id": "att-01"})
	for h.nextAny(t)["type"] != "result" {
	}
	h.exit(t, 0)
}
