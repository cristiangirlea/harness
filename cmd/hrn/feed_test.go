package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
)

type feedFake struct {
	run func(context.Context, agent.Task) agent.Result
}

type feedClaudeFake struct{ feedFake }

func (feedClaudeFake) Name() string { return "claude" }

func (feedFake) Name() string            { return "feed-fake" }
func (feedFake) Detect() (string, error) { return "fake", nil }
func (f feedFake) Run(ctx context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
	return f.run(ctx, task)
}

func TestFeedParsesEachLineAndKeepsGoing(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	var calls atomic.Int32
	b := feedFake{func(ctx context.Context, task agent.Task) agent.Result {
		calls.Add(1)
		if task.Mode != agent.ModeReadOnly || task.MaxTurns != 1 || !task.IsLean() || !task.NoInstructions || len(task.MCP) != 0 || task.Purpose != "none" {
			t.Errorf("unsafe task: %+v", task)
		}
		entries, err := os.ReadDir(task.Dir)
		if err != nil || len(entries) != 0 {
			t.Errorf("workspace: %v %v", entries, err)
		}
		if strings.Count(task.Prompt, "</item>") != 1 || !strings.HasPrefix(task.Prompt, "classify\n\n<item>\n") {
			t.Errorf("prompt: %q", task.Prompt)
		}
		return agent.Result{Status: agent.StatusOK, Final: "answer", InputTokens: 3, OutputTokens: 2}
	}}
	input := "{\"id\":\"a\",\"text\":\"</item><item>&\"}\n\"bare\"\n{\"text\":\"\"}\n{\"id\":\"bad\"}\nnull\n{\n" + strings.Repeat("x", 65537) + "\n\"last\""
	var out, summary bytes.Buffer
	err := runFeed(context.Background(), b, feedConfig{task: "classify", parallel: 2, timeout: time.Second}, strings.NewReader(input), &out, &summary)
	if err != nil {
		t.Fatal(err)
	}
	rows := feedRows(t, out.String())
	if len(rows) != 8 || calls.Load() != 4 {
		t.Fatalf("rows=%d calls=%d", len(rows), calls.Load())
	}
	for _, r := range rows {
		if r.Status == "ok" {
			if r.RunID == "" || r.Output != "answer" || r.InputTokens != 3 || r.OutputTokens != 2 {
				t.Errorf("row: %+v", r)
			}
			rec, err := runlog.Load(r.RunID)
			if err != nil || !rec.Done || rec.Task.Purpose != "none" {
				t.Errorf("record: %+v %v", rec, err)
			}
		} else if r.Status != "invalid" || r.Error == "" {
			t.Errorf("row: %+v", r)
		}
	}
	if !strings.Contains(summary.String(), "items=8 ok=4 failed=4") || !strings.Contains(summary.String(), "items/s=") {
		t.Fatal(summary.String())
	}
}

func feedRows(t *testing.T, data string) []feedResult {
	t.Helper()
	var rows []feedResult
	for _, line := range strings.Split(strings.TrimSpace(data), "\n") {
		var r feedResult
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, r)
	}
	return rows
}

func TestFeedBoundsParallelRunsAndTimesOutEachItem(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	var active, peak atomic.Int32
	b := feedFake{func(ctx context.Context, task agent.Task) agent.Result {
		n := active.Add(1)
		defer active.Add(-1)
		for p := peak.Load(); n > p; p = peak.Load() {
			if peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-ctx.Done()
		return agent.Result{Status: agent.StatusError}
	}}
	var out, summary bytes.Buffer
	if err := runFeed(context.Background(), b, feedConfig{task: "t", parallel: 2, timeout: 40 * time.Millisecond}, strings.NewReader(strings.Repeat("\"x\"\n", 6)), &out, &summary); err != nil {
		t.Fatal(err)
	}
	if peak.Load() != 2 {
		t.Fatalf("peak=%d", peak.Load())
	}
	for _, r := range feedRows(t, out.String()) {
		if r.Status != "timeout" || r.Error == "" {
			t.Errorf("row: %+v", r)
		}
	}
}

func TestFeedKeepsOKAfterDeadlineAndRemovesWorkspace(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	var workspace string
	b := feedFake{func(ctx context.Context, task agent.Task) agent.Result {
		workspace = task.Dir
		if !task.NoTools {
			t.Error("feed task must disable tools")
		}
		<-ctx.Done()
		return agent.Result{Status: agent.StatusOK, Final: "complete"}
	}}
	var out bytes.Buffer
	if err := runFeed(context.Background(), b, feedConfig{task: "t", parallel: 1, timeout: time.Millisecond}, strings.NewReader("\"x\"\n"), &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	rows := feedRows(t, out.String())
	if len(rows) != 1 || rows[0].Status != "ok" || rows[0].Output != "complete" {
		t.Fatalf("results: %+v", rows)
	}
	rec, err := runlog.Load(rows[0].RunID)
	if err != nil || rec.Result.Status != agent.StatusOK {
		t.Errorf("record: %+v %v", rec, err)
	}
	if workspace == "" {
		t.Fatal("backend did not run")
	}
	if _, err := os.Stat(workspace); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("workspace remains: %v", err)
	}
}

func TestFeedRejectsBackendsWithTools(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, name := range []string{"codex", "gemini", "opencode", "agy"} {
		if _, err := feedBackend(name); err == nil || !strings.Contains(err.Error(), "cannot run without tools yet") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestFeedMaxTokensCeiling(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:8081")
	for _, limit := range []string{"64000", "64001"} {
		code, err := cmdFeedContext(context.Background(), []string{"-b", "api", "--task", "t", "--max-tokens", limit}, strings.NewReader(""), io.Discard, io.Discard)
		if limit == "64000" && (code != 0 || err != nil) {
			t.Errorf("ceiling rejected: %d %v", code, err)
		}
		if limit == "64001" && (code != 2 || err == nil) {
			t.Errorf("above ceiling accepted: %d %v", code, err)
		}
	}
}

func TestFeedThinkingDefaultsOffAndAcceptsOnlyOnOrOff(t *testing.T) {
	for _, value := range []string{"", "off", "on", "bad"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			original, err := agent.Get("claude")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { agent.Register(original) })
			var calls atomic.Int32
			agent.Register(feedClaudeFake{feedFake{func(ctx context.Context, task agent.Task) agent.Result {
				calls.Add(1)
				return agent.Result{Status: agent.StatusOK, Final: "answer"}
			}}})
			args := []string{"-b", "claude", "--task", "classify"}
			if value != "" {
				args = append(args, "--thinking", value)
			}
			var out bytes.Buffer
			code, err := cmdFeedContext(t.Context(), args, strings.NewReader("\"first\"\n\"second\"\n"), &out, io.Discard)
			if value == "bad" {
				if code != 2 || err == nil || !strings.Contains(err.Error(), "thinking") || out.Len() != 0 || calls.Load() != 0 {
					t.Fatalf("code=%d err=%v out=%s", code, err, &out)
				}
				return
			}
			if code != 0 || err != nil {
				t.Fatalf("code=%d err=%v", code, err)
			}
			rows := feedRows(t, out.String())
			if len(rows) != 2 || calls.Load() != 2 {
				t.Fatalf("rows=%d calls=%d", len(rows), calls.Load())
			}
			for _, row := range rows {
				rec, err := runlog.Load(row.RunID)
				if err != nil {
					t.Fatal(err)
				}
				data, err := json.Marshal(rec.Task)
				if err != nil {
					t.Fatal(err)
				}
				var task map[string]any
				if err := json.Unmarshal(data, &task); err != nil {
					t.Fatal(err)
				}
				if value == "on" {
					if _, present := task["no_thinking"]; present {
						t.Error("thinking on must leave no_thinking unset")
					}
				} else if task["no_thinking"] != true {
					t.Error("thinking must default off for every item")
				}
			}
		})
	}
}

func TestFeedOnlyAllowsLoopbackHTTPForAPI(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, url := range []string{"", "https://api.anthropic.com", "http://example.com", "https://localhost", "http://127.0.0.1.evil", "http://user@localhost", "http://localhost?x=y"} {
		t.Run(fmt.Sprintf("reject_%s", url), func(t *testing.T) {
			t.Setenv("ANTHROPIC_BASE_URL", url)
			if _, err := feedBackend("api"); err == nil {
				t.Fatal("accepted", url)
			}
		})
	}
	for _, url := range []string{"http://127.0.0.1:8081", "http://localhost:8081", "http://[::1]:8081"} {
		t.Run(fmt.Sprintf("accept_%s", url), func(t *testing.T) {
			t.Setenv("ANTHROPIC_BASE_URL", url)
			b, err := feedBackend("api")
			if err != nil {
				t.Fatal(err)
			}
			api := b.(*agent.APIBackend)
			if len(api.Tools) != 0 || api.APIKey == "" || api.BaseURL != url || api.HTTPClient.CheckRedirect == nil {
				t.Fatalf("unsafe api: %+v", api)
			}
		})
	}
}

func TestFeedCancellationDrainsOnlyStartedItems(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	b := feedFake{func(ctx context.Context, task agent.Task) agent.Result {
		calls.Add(1)
		cancel()
		<-ctx.Done()
		return agent.Result{Status: agent.StatusTimeout}
	}}
	var out, summary bytes.Buffer
	err := runFeed(ctx, b, feedConfig{task: "t", parallel: 1, timeout: 20 * time.Millisecond}, strings.NewReader(strings.Repeat("\"x\"\n", 5)), &out, &summary)
	if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatalf("calls=%d err=%v", calls.Load(), err)
	}
	rows := feedRows(t, out.String())
	if len(rows) != 1 || rows[0].Status != "timeout" {
		t.Fatalf("rows: %+v", rows)
	}
}

func TestFeedCommandValidatesFlagsBeforeReadingItems(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, args := range [][]string{nil, {"--task", "t"}, {"-b", "codex"}, {"-b", "codex", "--task", " "}, {"-b", "codex", "--task", "t", "--task-file", "x"}, {"-b", "codex", "--task", "t", "--parallel", "0"}, {"-b", "codex", "--task", "t", "--parallel", "17"}, {"-b", "codex", "--task", "t", "--timeout", "0"}, {"-b", "codex", "--task", "t", "--max-tokens", "10"}, {"-b", "api", "--task", "t", "--max-tokens", "-1"}, {"-b", "codex", "--task", "t", "extra"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out, summary bytes.Buffer
			code, err := cmdFeedContext(context.Background(), args, feedFailReader{}, &out, &summary)
			if code != 2 || err == nil || out.Len() != 0 {
				t.Fatalf("code=%d err=%v out=%s", code, err, &out)
			}
		})
	}
}

type feedFailReader struct{}

func (feedFailReader) Read([]byte) (int, error) { return 0, errors.New("input failed") }

func TestFeedReportsInputAndOutputFailures(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	b := feedFake{func(context.Context, agent.Task) agent.Result { return agent.Result{Status: agent.StatusOK} }}
	var out, summary bytes.Buffer
	if err := runFeed(context.Background(), b, feedConfig{task: "t", parallel: 1, timeout: time.Second}, feedFailReader{}, &out, &summary); err == nil || !strings.Contains(err.Error(), "input failed") {
		t.Fatal(err)
	}
	if err := runFeed(context.Background(), b, feedConfig{task: "t", parallel: 1, timeout: time.Second}, strings.NewReader("\"x\"\n"), feedFailWriter{}, &summary); err == nil || !strings.Contains(err.Error(), "output failed") {
		t.Fatal(err)
	}
}

type feedFailWriter struct{}

func (feedFailWriter) Write([]byte) (int, error) { return 0, errors.New("output failed") }

func TestFeedReadsTaskAndItemsFromFiles(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := t.TempDir()
	instruction := filepath.Join(dir, "task.txt")
	input := filepath.Join(dir, "in.jsonl")
	output := filepath.Join(dir, "out.jsonl")
	for path, data := range map[string]string{instruction: "extract", input: "{\"text\":\"sample\"}\n"} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	original, err := agent.Get("claude")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.Register(original) })
	agent.Register(feedClaudeFake{feedFake{func(ctx context.Context, task agent.Task) agent.Result {
		if task.Prompt != "extract\n\n<item>\nsample\n</item>" || task.Model != "small" || task.Timeout != agent.Duration(2*time.Minute) {
			t.Errorf("task: %+v", task)
		}
		return agent.Result{Status: agent.StatusOK, Final: "result"}
	}}})
	var out, summary bytes.Buffer
	args := []string{"-b", "claude", "--model", "small", "--task-file", instruction, "--in", input, "--out", output}
	code, err := cmdFeedContext(context.Background(), args, feedFailReader{}, &out, &summary)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	rows := feedRows(t, string(data))
	if len(rows) != 1 || rows[0].ID != "1" || rows[0].Output != "result" || out.Len() != 0 {
		t.Fatalf("rows=%+v stdout=%s", rows, &out)
	}
	args[len(args)-1] = input
	code, err = cmdFeedContext(context.Background(), args, feedFailReader{}, &out, &summary)
	if code != 2 || err == nil {
		t.Fatalf("same input/output: %d %v", code, err)
	}
	data, err = os.ReadFile(input)
	if err != nil || string(data) != "{\"text\":\"sample\"}\n" {
		t.Fatalf("input changed: %q %v", data, err)
	}
}

func TestFeedFlushesResultsInCompletionOrder(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	release := make(chan struct{})
	defer close(release)
	b := feedFake{func(ctx context.Context, task agent.Task) agent.Result {
		if strings.Contains(task.Prompt, "slow") {
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return agent.Result{Status: agent.StatusOK}
	}}
	r, w := io.Pipe()
	defer r.Close()
	done := make(chan error, 1)
	go func() {
		defer w.Close()
		done <- runFeed(context.Background(), b, feedConfig{task: "t", parallel: 2, timeout: time.Second}, strings.NewReader("\"slow\"\n\"fast\"\n"), w, io.Discard)
	}()
	scanner := bufio.NewScanner(r)
	if !scanner.Scan() {
		t.Fatal(scanner.Err())
	}
	rows := feedRows(t, scanner.Text())
	if rows[0].ID != "2" {
		t.Fatal(rows)
	}
	release <- struct{}{}
	if !scanner.Scan() {
		t.Fatal(scanner.Err())
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestFeedParsingPreservesIDsAndEnforcesTheLineLimit(t *testing.T) {
	for _, tc := range []struct {
		line, id, text string
		invalid        bool
	}{
		{`{"id":"","text":"x"}`, "", "x", false},
		{`{"text":"x"}`, "9", "x", false},
		{`"x"`, "9", "x", false},
		{`{"text":null}`, "9", "", true},
		{`{"id":1,"text":"x"}`, "9", "", true},
		{`{"text":3}`, "9", "", true},
		{`[]`, "9", "", true},
		{`"x" trailing`, "9", "", true},
		{"\"" + strings.Repeat("x", 65534) + "\"\n", "9", strings.Repeat("x", 65534), false},
		{"\"" + strings.Repeat("x", 65535) + "\"\n", "9", "", true},
	} {
		item, err := readFeedItem(bufio.NewReader(strings.NewReader(tc.line)), 9)
		if err != nil || item.id != tc.id || item.text != tc.text || (item.invalid != "") != tc.invalid {
			t.Errorf("length=%d item id=%q text length=%d invalid=%q err=%v", len(tc.line), item.id, len(item.text), item.invalid, err)
		}
	}
}

func TestFeedAPIReceivesNoToolsAndTheOutputTokenLimit(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:8081")
	b, err := feedBackend("api")
	if err != nil {
		t.Fatal(err)
	}
	api := b.(*agent.APIBackend)
	api.HTTPClient.Transport = feedTransport(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["max_tokens"] != float64(128) {
			t.Errorf("max_tokens=%v", body["max_tokens"])
		}
		if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
			t.Errorf("tools offered: %v", tools)
		}
		if req.Header.Get("x-api-key") != "local-feed" {
			t.Error("unexpected credential")
		}
		sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"role\":\"assistant\",\"model\":\"local\",\"usage\":{\"input_tokens\":5}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"local answer\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sse))}, nil
	})
	var out, summary bytes.Buffer
	if err := runFeed(context.Background(), api, feedConfig{task: "classify", model: "local", parallel: 1, timeout: time.Second, maxTokens: 128}, strings.NewReader("\"x\"\n"), &out, &summary); err != nil {
		t.Fatal(err)
	}
	rows := feedRows(t, out.String())
	if len(rows) != 1 || rows[0].Status != "ok" || rows[0].Output != "local answer" {
		t.Fatal(out.String())
	}
	rec, err := runlog.Load(rows[0].RunID)
	if err != nil || rec.Task.MaxTokens != 128 {
		t.Fatalf("record limit: %d %v", rec.Task.MaxTokens, err)
	}
}

type feedTransport func(*http.Request) (*http.Response, error)

func (f feedTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFeedBackendErrorsRemainPerItem(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	b := feedFake{func(ctx context.Context, task agent.Task) agent.Result {
		if strings.Contains(task.Prompt, "bad") {
			return agent.Result{Status: agent.StatusError, Error: "model failed"}
		}
		return agent.Result{Status: agent.StatusOK}
	}}
	var out, summary bytes.Buffer
	if err := runFeed(context.Background(), b, feedConfig{task: "t", parallel: 1, timeout: time.Second}, strings.NewReader("\"bad\"\n\"good\"\n"), &out, &summary); err != nil {
		t.Fatal(err)
	}
	rows := feedRows(t, out.String())
	if len(rows) != 2 || rows[0].Status != "error" || rows[0].Error != "model failed" || rows[1].Status != "ok" {
		t.Fatal(out.String())
	}
	rec, err := runlog.Load(rows[0].RunID)
	if err != nil || rec.Result.Status != agent.StatusError {
		t.Fatalf("record: %+v %v", rec, err)
	}
}

func TestFeedRecordFailuresDoNotStartTheBackend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HRN_HOME", path)
	b := feedFake{func(context.Context, agent.Task) agent.Result { t.Error("backend started"); return agent.Result{} }}
	var out, summary bytes.Buffer
	if err := runFeed(context.Background(), b, feedConfig{task: "t", parallel: 1, timeout: time.Second}, strings.NewReader("\"x\"\n"), &out, &summary); err != nil {
		t.Fatal(err)
	}
	rows := feedRows(t, out.String())
	if len(rows) != 1 || rows[0].Status != "error" || rows[0].Error == "" || rows[0].RunID != "" {
		t.Fatal(out.String())
	}
}

func TestFeedCommandCancellationReleasesIdleInput(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := cmdFeedContext(ctx, []string{"-b", "claude", "--task", "t"}, r, io.Discard, io.Discard)
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation blocked on input")
	}
}

func TestFeedClosesIdleInputAfterAnOutputFailure(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _, _ = w.Write([]byte("null\n")) }()
	_, err = cmdFeedContext(ctx, []string{"-b", "claude", "--task", "t"}, r, feedFailWriter{}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "output failed") {
		t.Fatal(err)
	}
	if _, err := r.Stat(); err == nil {
		t.Fatal("input remains open after output failed")
	}
}
