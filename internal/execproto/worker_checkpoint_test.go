package execproto

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkerCheckpointAnswers(t *testing.T) {
	for _, mode := range []string{"ack", "nack", "wrong", "timeout", "closed", "pending"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"test\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"done\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			}))
			defer server.Close()
			t.Setenv("HRN_HOME", t.TempDir())
			t.Setenv("ANTHROPIC_BASE_URL", server.URL)
			t.Setenv("ANTHROPIC_API_KEY", "fake-credential-never-in-checkpoint")
			ir, iw := io.Pipe()
			or, ow := io.Pipe()
			defer ir.Close()
			defer iw.Close()
			defer or.Close()
			done := make(chan int, 1)
			go func() {
				defer ow.Close()
				done <- runWorkerWithTimeout(context.Background(), ir, ow, nil, 200*time.Millisecond)
			}()
			reader := bufio.NewReader(or)
			read := func() map[string]any {
				t.Helper()
				line, err := reader.ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				var v map[string]any
				if err := json.Unmarshal([]byte(line), &v); err != nil {
					t.Fatal(err)
				}
				return v
			}
			if read()["type"] != "hello" {
				t.Fatal("missing hello")
			}
			req := start(t.TempDir(), "api")
			if mode == "pending" {
				req["checkpoint"] = map[string]any{"pending": map[string]any{"kind": "model", "id": "turn-1"}}
			}
			if err := json.NewEncoder(iw).Encode(req); err != nil {
				t.Fatal(err)
			}
			if read()["type"] != "accepted" {
				t.Fatal("missing accepted")
			}
			count := 0
			for {
				frame := read()
				switch frame["type"] {
				case "checkpoint":
					count++
					raw, _ := json.Marshal(frame)
					if strings.Contains(string(raw), "fake-credential-never-in-checkpoint") {
						t.Fatal("credential in checkpoint")
					}
					cp := frame["checkpoint"].(map[string]any)
					seq := cp["sequence"].(float64)
					if seq != float64(count) {
						t.Fatalf("sequence %v, count %d", seq, count)
					}
					switch mode {
					case "ack":
						json.NewEncoder(iw).Encode(map[string]any{"type": "ack", "attempt_id": "att-01", "sequence": seq})
					case "nack":
						json.NewEncoder(iw).Encode(map[string]any{"type": "nack", "attempt_id": "att-01", "sequence": seq})
					case "wrong":
						json.NewEncoder(iw).Encode(map[string]any{"type": "ack", "attempt_id": "att-01", "sequence": seq + 1})
					case "closed":
						iw.Close()
					}
				case "result":
					result := frame["result"].(map[string]any)
					if mode == "ack" {
						if result["status"] != "ok" || count < 2 || calls.Load() != 1 {
							t.Fatalf("result=%v checkpoints=%d calls=%d", result, count, calls.Load())
						}
					} else if result["status"] != "error" || result["recovery_required"] != true || !strings.Contains(result["error"].(string), "execution requires recovery") || count > 1 || calls.Load() != 0 {
						t.Fatalf("result=%v checkpoints=%d calls=%d", result, count, calls.Load())
					}
					if <-done != 0 {
						t.Fatal("bad exit")
					}
					return
				}
			}
		})
	}
}
