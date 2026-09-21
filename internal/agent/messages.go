package agent

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
	"strconv"
	"strings"
	"time"
)

// Minimal Anthropic Messages API client on the standard library: one streaming
// POST /v1/messages with retries. Content blocks are kept as generic JSON
// objects so every block type the API returns (thinking signatures, fallback
// markers, future types) is echoed back unchanged in the next request.

const (
	apiVersion     = "2023-06-01"
	betaFallback   = "server-side-fallback-2026-07-01"
	betaOAuth      = "oauth-2025-04-20"
	betaTaskBudget = "task-budgets-2026-03-13"
)

type block = map[string]any

type apiMessage struct {
	Role    string  `json:"role"`
	Content []block `json:"content"`
}

type apiTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type apiRequest struct {
	Model        string       `json:"model"`
	MaxTokens    int          `json:"max_tokens"`
	Stream       bool         `json:"stream"`
	System       []block      `json:"system,omitempty"`
	Tools        []apiTool    `json:"tools,omitempty"`
	Thinking     block        `json:"thinking,omitempty"`
	CacheControl block        `json:"cache_control,omitempty"`
	OutputConfig block        `json:"output_config,omitempty"`
	Fallbacks    string       `json:"fallbacks,omitempty"`
	Messages     []apiMessage `json:"messages"`
	betas        []string     // sent as the anthropic-beta header
}

type apiUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

type apiResponse struct {
	Model       string  `json:"model"`
	Content     []block `json:"content"`
	StopReason  string  `json:"stop_reason"`
	StopDetails *struct {
		Category    string `json:"category"`
		Explanation string `json:"explanation"`
	} `json:"stop_details"`
	Usage apiUsage `json:"usage"`
	// invalidInput holds tool inputs that were not valid JSON, by tool_use id.
	invalidInput map[string]string
}

type apiClient struct {
	baseURL    string
	apiKey     string
	authToken  string
	http       *http.Client
	maxRetries int
}

// apiError is an HTTP or stream error from the API.
type apiError struct {
	Status  int
	Type    string
	Message string
	// RetryAfter is the server-requested delay, if any.
	RetryAfter time.Duration
}

func (e *apiError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("api %d %s: %s", e.Status, e.Type, e.Message)
	}
	return fmt.Sprintf("api stream %s: %s", e.Type, e.Message)
}

func (e *apiError) retryable() bool {
	switch {
	case e.Status == 408, e.Status == 409, e.Status == 429, e.Status >= 500:
		return true
	case e.Status == 0: // error event mid-stream
		return e.Type == "overloaded_error" || e.Type == "api_error"
	}
	return false
}

// credentials reads auth from the environment. ANTHROPIC_API_KEY wins over
// ANTHROPIC_AUTH_TOKEN (an OAuth bearer token).
func credentials() (apiKey, authToken, source string) {
	if k := os.Getenv("ANTHROPIC_API_KEY"); k != "" {
		return k, "", "ANTHROPIC_API_KEY"
	}
	if t := os.Getenv("ANTHROPIC_AUTH_TOKEN"); t != "" {
		return "", t, "ANTHROPIC_AUTH_TOKEN"
	}
	return "", "", ""
}

// stream sends one request and accumulates the streamed response, calling
// onDelta for text and thinking fragments and onBlock when a block completes.
// Failed attempts are retried with backoff before any output is emitted twice:
// a retry only happens if the failed attempt produced no content blocks.
func (c *apiClient) stream(ctx context.Context, req apiRequest, onDelta func(kind, text string), onBlock func(block)) (*apiResponse, error) {
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<(attempt-1)) * time.Second
			var ae *apiError
			if errors.As(lastErr, &ae) && ae.RetryAfter > 0 {
				delay = ae.RetryAfter
			}
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		resp, started, err := c.once(ctx, body, req.betas, onDelta, onBlock)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		var ae *apiError
		if ctx.Err() != nil || started || !errors.As(err, &ae) || !ae.retryable() {
			return resp, err
		}
	}
	return nil, lastErr
}

func (c *apiClient) once(ctx context.Context, body []byte, betas []string, onDelta func(kind, text string), onBlock func(block)) (resp *apiResponse, started bool, err error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.baseURL, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "text/event-stream")
	httpReq.Header.Set("anthropic-version", apiVersion)
	if c.apiKey != "" {
		httpReq.Header.Set("x-api-key", c.apiKey)
	} else if c.authToken != "" {
		httpReq.Header.Set("authorization", "Bearer "+c.authToken)
		betas = append(betas, betaOAuth)
	}
	if len(betas) > 0 {
		httpReq.Header.Set("anthropic-beta", strings.Join(betas, ","))
	}

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		// Connection failures are retryable.
		return nil, false, &apiError{Status: 503, Type: "connection_error", Message: err.Error()}
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusOK {
		return nil, false, readHTTPError(httpResp)
	}
	return parseSSE(httpResp.Body, onDelta, onBlock)
}

func readHTTPError(r *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	e := &apiError{Status: r.StatusCode, Type: "http_error", Message: strings.TrimSpace(string(data))}
	var env struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &env) == nil && env.Error.Type != "" {
		e.Type, e.Message = env.Error.Type, env.Error.Message
	}
	if s := r.Header.Get("retry-after"); s != "" {
		if secs, err := strconv.ParseFloat(s, 64); err == nil {
			e.RetryAfter = time.Duration(secs * float64(time.Second))
		}
	}
	return e
}

// sseEvent is the union of the stream event payloads the harness reads.
type sseEvent struct {
	Type         string          `json:"type"`
	Index        int             `json:"index"`
	Message      json.RawMessage `json:"message"`
	ContentBlock json.RawMessage `json:"content_block"`
	Delta        struct {
		Type        string          `json:"type"`
		Text        string          `json:"text"`
		Thinking    string          `json:"thinking"`
		Signature   string          `json:"signature"`
		PartialJSON string          `json:"partial_json"`
		Citation    json.RawMessage `json:"citation"`
		StopReason  string          `json:"stop_reason"`
		StopDetails json.RawMessage `json:"stop_details"`
	} `json:"delta"`
	Usage *apiUsage `json:"usage"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func parseSSE(r io.Reader, onDelta func(kind, text string), onBlock func(block)) (*apiResponse, bool, error) {
	resp := &apiResponse{}
	partial := map[int]*strings.Builder{} // tool input JSON per block index
	started := false
	done := false

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	var data strings.Builder
	dispatch := func() error {
		if data.Len() == 0 {
			return nil
		}
		raw := data.String()
		data.Reset()
		var ev sseEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			return fmt.Errorf("bad stream event: %w", err)
		}
		switch ev.Type {
		case "message_start":
			if err := json.Unmarshal(ev.Message, resp); err != nil {
				return err
			}
			resp.Content = resp.Content[:0]
		case "content_block_start":
			if ev.Index != len(resp.Content) {
				return fmt.Errorf("content block %d started out of order", ev.Index)
			}
			var b block
			if err := json.Unmarshal(ev.ContentBlock, &b); err != nil {
				return err
			}
			resp.Content = append(resp.Content, b)
			started = true
		case "content_block_delta":
			if ev.Index < 0 || ev.Index >= len(resp.Content) {
				return fmt.Errorf("delta for unknown content block %d", ev.Index)
			}
			b := resp.Content[ev.Index]
			switch ev.Delta.Type {
			case "text_delta":
				b["text"] = str(b["text"]) + ev.Delta.Text
				onDelta("text", ev.Delta.Text)
			case "thinking_delta":
				b["thinking"] = str(b["thinking"]) + ev.Delta.Thinking
				onDelta("thinking", ev.Delta.Thinking)
			case "signature_delta":
				b["signature"] = str(b["signature"]) + ev.Delta.Signature
			case "input_json_delta":
				if partial[ev.Index] == nil {
					partial[ev.Index] = &strings.Builder{}
				}
				partial[ev.Index].WriteString(ev.Delta.PartialJSON)
			case "citations_delta":
				var cit any
				if json.Unmarshal(ev.Delta.Citation, &cit) == nil {
					list, _ := b["citations"].([]any)
					b["citations"] = append(list, cit)
				}
			}
		case "content_block_stop":
			if ev.Index < 0 || ev.Index >= len(resp.Content) {
				return fmt.Errorf("stop for unknown content block %d", ev.Index)
			}
			b := resp.Content[ev.Index]
			if p, ok := partial[ev.Index]; ok {
				input := json.RawMessage("{}")
				if s := strings.TrimSpace(p.String()); s != "" {
					input = json.RawMessage(s)
				}
				if !json.Valid(input) {
					// Keep the history valid JSON; the tool call itself reports the problem.
					b["input"] = map[string]any{}
					if resp.invalidInput == nil {
						resp.invalidInput = map[string]string{}
					}
					resp.invalidInput[str(b["id"])] = p.String()
				} else {
					b["input"] = input
				}
			}
			onBlock(b)
		case "message_delta":
			resp.StopReason = ev.Delta.StopReason
			if len(ev.Delta.StopDetails) > 0 && string(ev.Delta.StopDetails) != "null" {
				_ = json.Unmarshal(ev.Delta.StopDetails, &resp.StopDetails)
			}
			if ev.Usage != nil {
				// Counts in message_delta are cumulative; zero means "not sent".
				resp.Usage.OutputTokens = ev.Usage.OutputTokens
				if ev.Usage.InputTokens > 0 {
					resp.Usage.InputTokens = ev.Usage.InputTokens
				}
				if ev.Usage.CacheReadInputTokens > 0 {
					resp.Usage.CacheReadInputTokens = ev.Usage.CacheReadInputTokens
				}
				if ev.Usage.CacheCreationInputTokens > 0 {
					resp.Usage.CacheCreationInputTokens = ev.Usage.CacheCreationInputTokens
				}
			}
		case "message_stop":
			done = true
		case "error":
			return &apiError{Type: ev.Error.Type, Message: ev.Error.Message}
		}
		return nil
	}

	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if err := dispatch(); err != nil {
				return resp, started, err
			}
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
		// "event:" lines are redundant with the payload's type field.
	}
	if err := dispatch(); err != nil {
		return resp, started, err
	}
	if err := sc.Err(); err != nil {
		return resp, started, err
	}
	if !done {
		return resp, started, &apiError{Type: "api_error", Message: "stream ended before message_stop"}
	}
	return resp, started, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
