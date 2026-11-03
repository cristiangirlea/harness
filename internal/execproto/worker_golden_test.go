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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
)

type goldenRecord struct{ dir string }

func (r goldenRecord) ID() string               { return "run-01" }
func (r goldenRecord) Dir() string              { return r.dir }
func (r goldenRecord) Event(agent.Event)        {}
func (r goldenRecord) Err() error               { return nil }
func (r goldenRecord) Close(agent.Result) error { return nil }

type goldenLine struct {
	Direction string          `json:"direction"`
	Frame     json.RawMessage `json:"frame,omitempty"`
	Action    string          `json:"action,omitempty"`
}

func TestWorkerGoldenTranscripts(t *testing.T) {
	if os.Getenv("HRN_GOLDEN_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestWorkerGoldenTranscripts$")
		cmd.Env = append(os.Environ(), "HRN_GOLDEN_CHILD=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolated transcript test: %v\n%s", err, output)
		}
		return
	}
	for _, name := range []string{"saved", "nack", "timeout", "pipe_closed", "pending", "workspace_changed", "fenced", "non_loopback", "tools", "continuation"} {
		t.Run(name, func(t *testing.T) {
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"test\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"done\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			}))
			defer model.Close()
			home := t.TempDir()
			dir := t.TempDir()
			cmd := exec.Command("git", "init", "--quiet", dir)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git init: %v\n%s", err, output)
			}
			t.Setenv("HRN_HOME", home)
			t.Setenv("ANTHROPIC_API_KEY", "fixture")
			endpoint := model.URL
			if name == "non_loopback" {
				endpoint = "http://example.com"
			}
			t.Setenv("ANTHROPIC_BASE_URL", endpoint)
			in, input := io.Pipe()
			output, out := io.Pipe()
			defer in.Close()
			defer input.Close()
			defer output.Close()
			done := make(chan int, 1)
			go func() {
				defer out.Close()
				done <- runMode(context.Background(), in, out, func() time.Time { return time.Unix(123, 0).UTC() }, true, func(_ string, _ agent.Task, _, _ string, _ *runlog.RoutedBy, _ string) (record, error) {
					return goldenRecord{dir}, nil
				}, 100*time.Millisecond)
			}()
			var transcript bytes.Buffer
			write := func(line goldenLine) {
				t.Helper()
				if err := json.NewEncoder(&transcript).Encode(line); err != nil {
					t.Fatal(err)
				}
			}
			send := func(v any) {
				t.Helper()
				b, err := json.Marshal(v)
				if err != nil {
					t.Fatal(err)
				}
				write(goldenLine{Direction: "worker", Frame: b})
				if _, err = input.Write(append(b, '\n')); err != nil {
					t.Fatal(err)
				}
			}
			req := start(dir, "api")
			req["task"].(map[string]any)["prompt"] = "fixture task"
			req["task"].(map[string]any)["mode"] = "readonly"
			if name == "saved" {
				req["task"].(map[string]any)["max_cost_usd"] = 1.25
				req["task"].(map[string]any)["task_budget"] = 20000
			}
			if name == "pending" {
				req["checkpoint"] = map[string]any{"pending": map[string]any{"kind": "model", "id": "turn-1"}}
			}
			if name == "continuation" {
				req["task"].(map[string]any)["continue_run_id"] = "previous"
			}
			if name == "tools" {
				req["task"].(map[string]any)["tools"] = []string{"shell"}
			}
			reader := bufio.NewReader(output)
			for {
				line, err := reader.ReadBytes('\n')
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				var frame map[string]any
				if err := json.Unmarshal(line, &frame); err != nil {
					t.Fatal(err)
				}
				write(goldenLine{Direction: "hrn", Frame: bytes.TrimSpace(line)})
				switch frame["type"] {
				case "hello":
					send(req)
				case "checkpoint":
					switch name {
					case "saved":
						send(map[string]any{"type": "ack", "attempt_id": "att-01", "sequence": frame["checkpoint"].(map[string]any)["sequence"]})
					case "nack", "workspace_changed", "fenced":
						send(map[string]any{"type": "nack", "attempt_id": "att-01", "sequence": frame["checkpoint"].(map[string]any)["sequence"]})
					case "pipe_closed":
						write(goldenLine{Direction: "worker", Action: "close"})
						input.Close()
					case "timeout":
						write(goldenLine{Direction: "worker", Action: "timeout"})
					default:
						t.Fatalf("unexpected checkpoint in %s", name)
					}
				}
			}
			code := <-done
			wantCode := 0
			if name == "non_loopback" || name == "tools" || name == "continuation" {
				wantCode = 2
			}
			if code != wantCode {
				t.Fatalf("exit %d", code)
			}
			actual := normalizeTranscript(transcript.Bytes(), dir)
			path := filepath.Join("testdata", "worker", name+".jsonl")
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, actual, 0644); err != nil {
					t.Fatal(err)
				}
			}
			expected, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(actual, expected) {
				t.Fatalf("transcript differs from %s\ngot:\n%s", path, actual)
			}
		})
	}
}

// normalizeTranscript replaces machine-dependent paths, platform details, and execution values.
func normalizeTranscript(actual []byte, dir string) []byte {
	actual = regexp.MustCompile(`("hrn":\{[^}]*"version":")[^"]*"`).ReplaceAll(actual, []byte(`${1}<variable>"`))
	actual = bytes.ReplaceAll(actual, []byte(strings.ReplaceAll(dir, `\`, `\\`)), []byte("<workspace>"))
	actual = regexp.MustCompile(`OS: [^\\]*\\nShell for run_command: [^\\]*`).ReplaceAll(actual, []byte(`OS: <platform>\nShell for run_command: <shell>`))
	for _, pattern := range []string{`"pid":[0-9]+`, `"duration_ns":[0-9]+`, `"started":"[^"]+"`, `"t":"[^"]+"`, `"task_digest":"[^"]+"`, `"go":"[^"]+"`} {
		actual = regexp.MustCompile(pattern).ReplaceAll(actual, []byte(strings.Split(pattern, ":")[0]+`:"<variable>"`))
	}
	return actual
}
