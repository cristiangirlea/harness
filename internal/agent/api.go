package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/cristiangirlea/harness/internal/tools"
)

const DefaultAPIModel = "claude-opus-5"

// APIBackend is the harness's own agent loop over the Anthropic Messages API,
// using the client-side tools in package tools.
type APIBackend struct {
	Tools []tools.Tool
	// BaseURL and APIKey override ANTHROPIC_BASE_URL and the env credentials.
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
	MaxRetries int
}

func NewAPIBackend() *APIBackend {
	return &APIBackend{Tools: tools.All(), MaxRetries: 3}
}

func (b *APIBackend) Name() string { return "api" }

func (b *APIBackend) Detect() (string, error) {
	if b.APIKey != "" {
		return "configured key", nil
	}
	if _, _, source := credentials(); source != "" {
		return source, nil
	}
	return "", errors.New("set ANTHROPIC_API_KEY (or ANTHROPIC_AUTH_TOKEN)")
}

func (b *APIBackend) client() (*apiClient, error) {
	c := &apiClient{baseURL: b.BaseURL, http: b.HTTPClient, maxRetries: b.MaxRetries}
	if c.baseURL == "" {
		c.baseURL = os.Getenv("ANTHROPIC_BASE_URL")
	}
	if c.baseURL == "" {
		c.baseURL = "https://api.anthropic.com"
	}
	if c.http == nil {
		// No overall timeout: responses stream for as long as the model works.
		c.http = &http.Client{}
	}
	c.apiKey = b.APIKey
	if c.apiKey == "" {
		c.apiKey, c.authToken, _ = credentials()
	}
	if c.apiKey == "" && c.authToken == "" {
		return nil, errors.New("no API credentials: set ANTHROPIC_API_KEY (or ANTHROPIC_AUTH_TOKEN)")
	}
	return c, nil
}

const systemPrompt = `You are an autonomous software engineering agent running inside a custom harness.

You work in a single workspace directory and act only through the provided tools. Paths are relative to the workspace root.

How to work:
- Investigate before changing: read the relevant files and search for existing patterns, then make focused changes that match the surrounding code.
- Prefer edit_file for changes to existing files; use write_file for new files.
- After changing code, verify it (build, tests, or running it) with run_command when that is possible.
- A tool call may be denied by the user's permission settings. Do not retry a denied call unchanged; adapt or explain what you need.
- When the task is done, reply with a short summary of what changed and how you verified it. If you could not finish, say what is blocking.`

func (b *APIBackend) Run(ctx context.Context, task Task, emit func(Event)) Result {
	started := time.Now()
	model := task.Model
	if model == "" {
		model = DefaultAPIModel
	}
	res := Result{Backend: b.Name(), Model: model, CostEstimate: true}
	fail := func(status Status, err error) Result {
		res.Status = status
		res.Error = err.Error()
		emit(Event{Time: time.Now(), Kind: EventError, Text: err.Error()})
		return finish(&res, started)
	}

	client, err := b.client()
	if err != nil {
		return fail(StatusError, err)
	}
	ws, err := tools.NewWorkspace(task.Dir)
	if err != nil {
		return fail(StatusError, err)
	}
	maxTurns := task.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 50
	}

	toolset := b.Tools
	if len(task.MCP) > 0 {
		clients, extra := startMCP(ctx, task, ws.Root, emit)
		defer func() {
			for _, c := range clients {
				_ = c.Close()
			}
		}()
		toolset = append(append([]tools.Tool(nil), toolset...), extra...)
	}

	byName := map[string]tools.Tool{}
	var apiTools []apiTool
	for _, t := range toolset {
		if task.Mode == ModeReadOnly && t.Effect != tools.EffectRead {
			continue // don't offer tools the mode can never allow
		}
		byName[t.Name] = t
		schema := t.Schema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": t.Properties}
			if len(t.Required) > 0 {
				schema["required"] = t.Required
			}
		}
		apiTools = append(apiTools, apiTool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}

	shell, _ := tools.Shell()
	env := fmt.Sprintf("Workspace root: %s\nOS: %s\nShell for run_command: %s\nPermission mode: %s",
		ws.Root, runtime.GOOS, shell, task.Mode)
	ephemeral := block{"type": "ephemeral"}

	system := []block{{"type": "text", "text": systemPrompt}}
	if !task.NoInstructions {
		if text, names, err := loadInstructions(ws.Root); err != nil {
			// Missing files are normal; an unreadable one is worth saying out
			// loud, but it must not fail the run.
			emit(Event{Time: time.Now(), Kind: EventError, Text: "project instructions: " + err.Error()})
		} else if text != "" {
			system = append(system, block{"type": "text", "text": text})
			emit(Event{Time: time.Now(), Kind: EventInfo,
				Text: fmt.Sprintf("instructions: %s (%.1f KB)", strings.Join(names, ", "), float64(len(text))/1024)})
		}
	}
	// Constant for the whole run, so this breakpoint caches tools, prompt and
	// instructions together.
	system = append(system, block{"type": "text", "text": env, "cache_control": ephemeral})

	req := apiRequest{
		Model:     model,
		MaxTokens: 64000,
		System:    system,
		Tools:     apiTools,
		Thinking:  block{"type": "adaptive", "display": "summarized"},
		// Top-level breakpoint follows the conversation so history is cached too.
		CacheControl: ephemeral,
		Messages: []apiMessage{
			{Role: "user", Content: []block{{"type": "text", "text": task.Prompt}}},
		},
	}
	if task.Effort != "" {
		req.OutputConfig = block{"effort": task.Effort}
	}
	if supportsDefaultFallbacks(model) {
		// A refusal is re-served by a fallback model inside the same call.
		req.Fallbacks = "default"
		req.betas = append(req.betas, betaFallback)
	}

	onDelta := func(kind, text string) {
		k := EventText
		if kind == "thinking" {
			k = EventThinking
		}
		emit(Event{Kind: k, Text: text, Partial: true})
	}
	onBlock := func(bl block) {
		now := time.Now()
		switch bl["type"] {
		case "text":
			emit(Event{Time: now, Kind: EventText, Text: str(bl["text"]), Streamed: true})
		case "thinking":
			if t := str(bl["thinking"]); t != "" {
				emit(Event{Time: now, Kind: EventThinking, Text: t, Streamed: true})
			}
		}
	}

	for turn := 1; ; turn++ {
		if turn > maxTurns {
			return fail(StatusMaxTurns, fmt.Errorf("stopped after %d turns", maxTurns))
		}
		res.Turns = turn

		msg, err := client.stream(ctx, req, onDelta, onBlock)
		if msg != nil {
			res.InputTokens += msg.Usage.InputTokens
			res.OutputTokens += msg.Usage.OutputTokens
			res.CacheRead += msg.Usage.CacheReadInputTokens
			res.CacheWrite += msg.Usage.CacheCreationInputTokens
			res.CostUSD = estimateCost(model, res)
		}
		if err != nil {
			if ctx.Err() != nil {
				return fail(StatusCanceled, ctx.Err())
			}
			return fail(StatusError, err)
		}
		if msg.Model != "" {
			res.Model = msg.Model
		}
		// History is append-only: the assistant turn goes back exactly as received.
		req.Messages = append(req.Messages, apiMessage{Role: "assistant", Content: msg.Content})

		var calls []block
		var text strings.Builder
		for _, bl := range msg.Content {
			switch bl["type"] {
			case "text":
				text.WriteString(str(bl["text"]))
			case "tool_use":
				calls = append(calls, bl)
			}
		}

		switch msg.StopReason {
		case "refusal":
			category, explanation := "", ""
			if msg.StopDetails != nil {
				category, explanation = msg.StopDetails.Category, msg.StopDetails.Explanation
			}
			return fail(StatusRefused, fmt.Errorf("model declined (%s): %s", category, explanation))
		case "max_tokens":
			return fail(StatusError, errors.New("response hit max_tokens"))
		case "pause_turn":
			continue
		case "tool_use":
			results := b.runTools(ctx, ws, task, calls, msg.invalidInput, byName, emit)
			req.Messages = append(req.Messages, apiMessage{Role: "user", Content: results})
		default:
			res.Status = StatusOK
			res.Final = strings.TrimSpace(text.String())
			return finish(&res, started)
		}
	}
}

func (b *APIBackend) runTools(ctx context.Context, ws *tools.Workspace, task Task, calls []block, invalid map[string]string, byName map[string]tools.Tool, emit func(Event)) []block {
	results := make([]block, len(calls))
	var emitMu sync.Mutex
	result := func(i int, name, out string, isError bool) {
		emitMu.Lock()
		emit(Event{Time: time.Now(), Kind: EventToolResult, Tool: name, Text: out, IsError: isError})
		emitMu.Unlock()
		r := block{"type": "tool_result", "tool_use_id": str(calls[i]["id"]), "content": out}
		if isError {
			r["is_error"] = true
		}
		results[i] = r
	}

	type job struct {
		i     int
		tool  tools.Tool
		input json.RawMessage
	}
	var reads, others []job

	// Approvals are decided up front and in order, so prompts never interleave.
	for i, call := range calls {
		name, id := str(call["name"]), str(call["id"])
		input, _ := json.Marshal(call["input"])
		t, ok := byName[name]
		summary := name
		if ok && t.Summary != nil {
			summary = t.Summary(input)
		}
		emit(Event{Time: time.Now(), Kind: EventToolCall, Tool: name, Input: input, Text: summary})
		switch {
		case !ok:
			result(i, name, fmt.Sprintf("unknown tool %q", name), true)
		case invalid[id] != "":
			result(i, name, "tool input was not valid JSON: "+invalid[id], true)
		case !allowed(task, t, summary):
			result(i, name, "Permission denied by the user's harness settings.", true)
		case t.Effect == tools.EffectRead:
			reads = append(reads, job{i, t, input})
		default:
			others = append(others, job{i, t, input})
		}
	}

	run := func(j job) {
		out, err := j.tool.Run(ctx, ws, j.input)
		if err != nil {
			if out == "" {
				out = err.Error()
			} else {
				out += "\n" + err.Error()
			}
		}
		result(j.i, j.tool.Name, out, err != nil)
	}

	// Reads have no side effects and run in parallel; writes and commands run
	// one at a time in the order the model asked for them.
	var wg sync.WaitGroup
	for _, j := range reads {
		wg.Add(1)
		go func() { defer wg.Done(); run(j) }()
	}
	wg.Wait()
	for _, j := range others {
		run(j)
	}
	return results
}

func allowed(task Task, t tools.Tool, summary string) bool {
	switch {
	case t.Effect == tools.EffectRead:
		return true
	case task.Mode == ModeYolo:
		return true
	case task.Mode == ModeAuto && t.Effect == tools.EffectWrite:
		return true
	case task.Mode == ModeReadOnly:
		return false
	}
	if task.Approve == nil {
		return false
	}
	return task.Approve(ApprovalRequest{Tool: t.Name, Summary: summary})
}

func supportsDefaultFallbacks(model string) bool {
	return model == "claude-opus-5" || model == "claude-fable-5-1"
}

// price per million tokens: input, output. Cache reads bill at 0.1x input and
// 5-minute cache writes at 1.25x input.
var prices = map[string][2]float64{
	"claude-fable-5-1": {10, 50},
	"claude-fable-5":   {10, 50},
	"claude-opus-5":    {5, 25},
	"claude-opus-4-8":  {5, 25},
	"claude-sonnet-5":  {2, 10},
	"claude-haiku-4-5": {1, 5},
}

func estimateCost(model string, r Result) float64 {
	p, ok := prices[model]
	if !ok {
		return 0
	}
	in := float64(r.InputTokens) + 0.1*float64(r.CacheRead) + 1.25*float64(r.CacheWrite)
	return (in*p[0] + float64(r.OutputTokens)*p[1]) / 1e6
}
