package platform

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type transcriptLine struct {
	Direction string          `json:"direction"`
	Frame     json.RawMessage `json:"frame"`
	Action    string          `json:"action"`
}

func TestWorkerTranscriptReplay(t *testing.T) {
	cases := map[string]workerOutcome{"saved": outcomeSaved, "nack": outcomeRecovery, "timeout": outcomeRecovery, "pipe_closed": outcomeRecovery, "pending": outcomeRecovery, "workspace_changed": outcomeRecovery, "fenced": outcomeRecovery, "non_loopback": outcomeRefused, "tools": outcomeRefused, "continuation": outcomeRefused}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			f, err := os.Open(filepath.Join("..", "..", "internal", "execproto", "testdata", "worker", name+".jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			var input, expected bytes.Buffer
			var start json.RawMessage
			var attempt string
			var callbacks int
			s := bufio.NewScanner(f)
			s.Buffer(make([]byte, 4096), 4<<20)
			for s.Scan() {
				var line transcriptLine
				if err := json.Unmarshal(s.Bytes(), &line); err != nil {
					t.Fatal(err)
				}
				if line.Direction == "hrn" {
					input.Write(line.Frame)
					input.WriteByte('\n')
				} else if len(line.Frame) > 0 {
					if start == nil {
						start = append(json.RawMessage(nil), line.Frame...)
						var v struct {
							Attempt string `json:"attempt_id"`
						}
						if err := json.Unmarshal(start, &v); err != nil {
							t.Fatal(err)
						}
						attempt = v.Attempt
					}
					expected.Write(line.Frame)
					expected.WriteByte('\n')
				}
			}
			if err := s.Err(); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			c := workerClient{input: &input, output: &output, attemptID: attempt, start: start, checkpoint: func(cp json.RawMessage) checkpointDecision {
				callbacks++
				if name == "timeout" || name == "pipe_closed" {
					return checkpointNoAnswer
				}
				if name == "saved" {
					return checkpointSaved
				}
				return checkpointRejected
			}}
			got, err := c.run()
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("outcome %q, want %q", got, want)
			}
			if !bytes.Equal(output.Bytes(), expected.Bytes()) {
				t.Fatalf("frames = %s, want %s", output.Bytes(), expected.Bytes())
			}
			if (name == "saved" && callbacks != 3) || ((name == "nack" || name == "timeout" || name == "pipe_closed" || name == "workspace_changed" || name == "fenced") && callbacks != 1) || ((name == "pending" || want == outcomeRefused) && callbacks != 0) {
				t.Fatalf("callbacks: %d", callbacks)
			}
		})
	}
}

func TestWorkerClientCancelAndFailures(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		var out bytes.Buffer
		c := workerClient{output: &out, attemptID: "a"}
		if err := c.cancel(); err != nil {
			t.Fatal(err)
		}
		if out.String() != "{\"attempt_id\":\"a\",\"type\":\"cancel\"}\n" {
			t.Fatal(out.String())
		}
	})
	t.Run("missing result", func(t *testing.T) {
		c := workerClient{input: bytes.NewBufferString("{\"v\":1,\"seq\":1,\"type\":\"hello\"}\n"), output: &bytes.Buffer{}, attemptID: "a", start: json.RawMessage(`{"type":"start"}`)}
		got, err := c.run()
		if err == nil || got != outcomeRecovery {
			t.Fatalf("%q %v", got, err)
		}
	})
}
