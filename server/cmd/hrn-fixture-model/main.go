package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

// This deterministic local endpoint supplies model responses to disposable
// worker tests. It never calls a model provider.
func main() {
	address := os.Getenv("HRN_FIXTURE_LISTEN")
	if address == "" {
		address = "127.0.0.1:9090"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		lines := []string{
			"event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_fixture","type":"message","role":"assistant","model":"claude-opus-5","content":[],"stop_reason":null,"usage":{"input_tokens":100,"output_tokens":1}}}` + "\n\n",
			"event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n",
			"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"fixture completed"}}` + "\n\n",
			"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":0}` + "\n\n",
			"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20}}` + "\n\n",
			"event: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n",
		}
		for _, line := range lines {
			fmt.Fprint(w, line)
		}
	})
	server := &http.Server{Addr: address, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(server.ListenAndServe())
}
