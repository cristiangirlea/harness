package execproto

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
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
		{"continuation", "api", "http://localhost", `,"continue_run_id":"previous"`},
		{"fresh true", "api", "http://localhost", `,"fresh":true`},
		{"fresh false", "api", "http://localhost", `,"fresh":false`},
		{"retrieval refresh", "api", "http://localhost", `,"retrieval_refresh":true`},
		{"retrieval refresh false", "api", "http://localhost", `,"retrieval_refresh":false`},
		{"retrieval mode", "api", "http://localhost", `,"retrieval":"required"`},
		{"retrieval tenant", "api", "http://localhost", `,"retrieval_tenant":"tenant"`},
		{"empty retrieval tenant", "api", "http://localhost", `,"retrieval_tenant":""`},
		{"retrieval project", "api", "http://localhost", `,"retrieval_project":"project"`},
		{"retrieval revision", "api", "http://localhost", `,"retrieval_revision":"revision"`},
		{"retrieval version", "api", "http://localhost", `,"retrieval_version":"version"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			t.Setenv("ANTHROPIC_BASE_URL", tc.endpoint)
			request := start(t.TempDir(), tc.backend)
			var extra map[string]any
			if err := json.Unmarshal([]byte("{"+strings.TrimPrefix(tc.extra, ",")+"}"), &extra); err != nil {
				t.Fatal(err)
			}
			for key, value := range extra {
				request["task"].(map[string]any)[key] = value
			}
			task, _ := json.Marshal(request)
			input := string(task) + "\n"
			var out bytes.Buffer
			if code := RunWorker(context.Background(), strings.NewReader(input), &out, nil); code != 2 || !strings.Contains(out.String(), `"type":"rejected"`) {
				t.Fatalf("code=%d output=%s", code, out.String())
			}
			if tc.name != "tools requested" && !strings.Contains(out.String(), `"code":"unsupported"`) {
				t.Fatalf("expected unsupported: %s", out.String())
			}
		})
	}
}

func TestWorkerRejectsInvalidLimits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
		value any
		want  string
	}{
		{"negative cost", "max_cost_usd", -1, "max cost -1.00 is negative"},
		{"negative budget", "task_budget", -1, "task budget -1 is below the API minimum"},
		{"budget below minimum", "task_budget", agent.MinTaskBudget - 1, "task budget 19999 is below the API minimum"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			t.Setenv("ANTHROPIC_BASE_URL", "http://localhost")
			req := start(t.TempDir(), "api")
			req["task"].(map[string]any)[tc.field] = tc.value
			input, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			code := RunWorker(context.Background(), bytes.NewReader(append(input, '\n')), &out, nil)
			if code != 2 || !strings.Contains(out.String(), `"type":"rejected"`) || !strings.Contains(out.String(), `"code":"bad_request"`) || !strings.Contains(out.String(), tc.want) {
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
	req := start(t.TempDir(), "api")
	req["task"].(map[string]any)["task_budget"] = 20000
	req["task"].(map[string]any)["max_cost_usd"] = 2.5
	req["task"].(map[string]any)["timeout"] = "1m"
	input, _ := json.Marshal(req)
	reader, writer := io.Pipe()
	output, outputWriter := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	defer output.Close()
	done := make(chan int, 1)
	go func() { defer outputWriter.Close(); done <- RunWorker(context.Background(), reader, outputWriter, nil) }()
	go func() { fmt.Fprintln(writer, string(input)) }()
	scanner := bufio.NewScanner(output)
	accepted := false
	for scanner.Scan() {
		var frame map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			t.Fatal(err)
		}
		if frame["type"] == "accepted" {
			accepted = true
		}
		if frame["type"] == "checkpoint" {
			cp := frame["checkpoint"].(map[string]any)
			if err := json.NewEncoder(writer).Encode(map[string]any{"type": "ack", "attempt_id": "att-01", "sequence": cp["sequence"]}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if code := <-done; code != 0 || !accepted || scanner.Err() != nil {
		t.Fatalf("code=%d accepted=%v err=%v", code, accepted, scanner.Err())
	}

	request := <-requests
	var outputConfig struct {
		TaskBudget struct {
			Total int `json:"total"`
		} `json:"task_budget"`
	}
	if err := json.Unmarshal(request["output_config"], &outputConfig); err != nil || outputConfig.TaskBudget.Total != 20000 {
		t.Fatalf("task budget not sent: %s, err=%v", request["output_config"], err)
	}
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

func TestWorkerCostCeiling(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ceiling float64
		want    agent.Status
	}{{"ceiling", 0.000001, agent.StatusBudget}, {"default", 0, agent.StatusMaxTurns}} {
		t.Run(tc.name, func(t *testing.T) {
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"claude-opus-4-6\",\"usage\":{\"input_tokens\":1000,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"done\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"pause_turn\"},\"usage\":{\"output_tokens\":100}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			}))
			defer model.Close()
			t.Setenv("HRN_HOME", t.TempDir())
			t.Setenv("ANTHROPIC_BASE_URL", model.URL)
			t.Setenv("ANTHROPIC_API_KEY", "fixture")
			req := start(t.TempDir(), "api")
			task := req["task"].(map[string]any)
			task["model"] = "claude-opus-4-6"
			if tc.ceiling != 0 {
				task["max_cost_usd"] = tc.ceiling
			}
			input, writer := io.Pipe()
			output, out := io.Pipe()
			defer input.Close()
			defer writer.Close()
			defer output.Close()
			done := make(chan int, 1)
			go func() { defer out.Close(); done <- RunWorker(context.Background(), input, out, nil) }()
			go json.NewEncoder(writer).Encode(req)
			var result agent.Result
			scanner := bufio.NewScanner(output)
			for scanner.Scan() {
				var f struct {
					Type       string `json:"type"`
					Checkpoint struct {
						Sequence int64 `json:"sequence"`
					} `json:"checkpoint"`
					Result agent.Result `json:"result"`
				}
				if err := json.Unmarshal(scanner.Bytes(), &f); err != nil {
					t.Fatal(err)
				}
				if f.Type == "checkpoint" {
					json.NewEncoder(writer).Encode(map[string]any{"type": "ack", "attempt_id": "att-01", "sequence": f.Checkpoint.Sequence})
				}
				if f.Type == "result" {
					result = f.Result
				}
			}
			if code := <-done; code != 0 || result.Status != tc.want {
				t.Fatalf("code=%d result=%+v", code, result)
			}
		})
	}
}
