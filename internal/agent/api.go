package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
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
	ctx, cancel := withTimeout(ctx, task)
	defer cancel()
	model := task.Model
	if model == "" {
		model = DefaultAPIModel
	}
	res := Result{Backend: b.Name(), Model: model, CostEstimate: true}
	if _, priced := priceOf(model); !priced {
		// Without a price the cost reads as zero, which looks like free.
		text := fmt.Sprintf("no price known for %s: its cost is not estimated", model)
		if task.MaxCostUSD > 0 {
			text = fmt.Sprintf("max_cost cannot be enforced: no price known for %s", model)
		}
		emit(Event{Time: time.Now(), Kind: EventInfo, Text: text})
	}
	if task.CompactWindow != "" {
		// The api loop has no compaction of its own; context editing, on by
		// default, is what keeps its requests small.
		emit(Event{Time: time.Now(), Kind: EventInfo, Text: "compact_window: not enforced by the api backend"})
	}
	// req is built below; saveConversation closes over it so every exit path
	// leaves a resumable history behind.
	var req apiRequest
	saveConversation := func() {
		if task.RunDir == "" || task.Durable != nil {
			return
		}
		msgs := resumable(req.Messages)
		if len(msgs) == 0 {
			// A run that failed before it built a request has nothing to
			// continue from, and an empty file would look like it had.
			return
		}
		conv := Conversation{Model: res.Model, Messages: msgs, Retrieval: task.RetrievalState}
		// Compact: this file is read back by hrn, not by people, and it already
		// repeats every tool result the transcript holds.
		if data, err := json.Marshal(conv); err == nil {
			_ = os.WriteFile(filepath.Join(task.RunDir, "messages.json"), data, 0o644)
		}
	}
	fail := func(status Status, err error) Result {
		res.Status = status
		res.Error = err.Error()
		emit(Event{Time: time.Now(), Kind: EventError, Text: err.Error()})
		saveConversation()
		return finish(&res, started)
	}

	if task.Durable != nil && len(task.MCP) != 0 {
		return fail(StatusError, errors.New("durable MCP execution requires an isolated executor; local MCP subprocesses are unsupported"))
	}
	client, err := b.client()
	if err != nil {
		return fail(StatusError, err)
	}
	ws, err := tools.NewWorkspace(task.Dir)
	if err != nil {
		return fail(StatusError, err)
	}
	ws.AllowDangerous = task.AllowDangerous
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
	guard := "on (a few destructive or publishing command lines are refused)"
	if task.AllowDangerous {
		guard = "off (the operator passed --allow-dangerous)"
	}
	env := fmt.Sprintf("Workspace root: %s\nOS: %s\nShell for run_command: %s\nPermission mode: %s\nCommand guard: %s",
		ws.Root, runtime.GOOS, shell, task.Mode, guard)
	ephemeral := block{"type": "ephemeral"}

	system := []block{{"type": "text", "text": systemPrompt}}
	if !task.NoInstructions {
		if text, names, truncated, err := loadInstructions(ws); err != nil {
			// Missing files are normal; an unreadable one is worth saying out
			// loud, but it must not fail the run.
			emit(Event{Time: time.Now(), Kind: EventError, Text: "project instructions: " + err.Error()})
		} else if text != "" {
			system = append(system, block{"type": "text", "text": text})
			note := ""
			if truncated {
				note = ", truncated"
			}
			emit(Event{Time: time.Now(), Kind: EventInfo,
				Text: fmt.Sprintf("instructions: %s (%.1f KB%s)", strings.Join(names, ", "), float64(len(text))/1024, note)})
		}
	}
	// Constant for the whole run, so this breakpoint caches tools, prompt and
	// instructions together.
	system = append(system, block{"type": "text", "text": env, "cache_control": ephemeral})

	req = apiRequest{
		Model:     model,
		MaxTokens: 64000,
		System:    system,
		Tools:     apiTools,
		Thinking:  block{"type": "adaptive", "display": "summarized"},
		// Top-level breakpoint follows the conversation so history is cached too.
		CacheControl: ephemeral,
		Messages:     resumeMessages(task),
	}
	if task.Effort != "" {
		req.OutputConfig = block{"effort": task.Effort}
	}
	if task.TaskBudget > 0 {
		if req.OutputConfig == nil {
			req.OutputConfig = block{}
		}
		// remaining stays unset: hrn resends the whole history every turn, so
		// the server tracks the countdown better than the client could.
		req.OutputConfig["task_budget"] = block{"type": "tokens", "total": task.TaskBudget}
		req.betas = append(req.betas, betaTaskBudget)
	}
	if !task.NoContextEdits {
		// The client history stays append-only and the whole of it is still
		// uploaded; what the server clears is what the model is given, which
		// is what fills the context window and what the input is billed on. A
		// clear invalidates the cached prefix from that point, which is the
		// trade, and it is unmeasured against a real long run.
		req.ContextManagement = block{"edits": []block{{"type": "clear_tool_uses_20250919"}}}
		req.betas = append(req.betas, betaContextEdits)
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

	durable, err := prepareDurable(task, &req, &res)
	if err != nil {
		res.RecoveryRequired = errors.Is(err, ErrRecoveryRequired)
		return fail(StatusError, err)
	}
	firstTurn := 1
	if durable != nil {
		// Generic HTTP retries can repeat requests whose effects are unknown.
		// The local path retains its historical retry behavior.
		client.maxRetries = 0
		firstTurn = durable.checkpoint.NextTurn
		if durable.checkpoint.Finished {
			return durable.checkpoint.Result
		}
		if !durable.checkpoint.Deadline.IsZero() {
			var deadlineCancel context.CancelFunc
			ctx, deadlineCancel = context.WithDeadline(ctx, durable.checkpoint.Deadline)
			defer deadlineCancel()
		}
	}
	durableFailure := func(err error) Result {
		res.RecoveryRequired = true
		return fail(StatusError, fmt.Errorf("durable execution: %w", err))
	}
	for turn := firstTurn; ; turn++ {
		if turn > maxTurns {
			return fail(StatusMaxTurns, fmt.Errorf("stopped after %d turns", maxTurns))
		}
		if task.MaxCostUSD > 0 && res.CostUSD >= task.MaxCostUSD && (durable == nil || len(durable.checkpoint.Response) == 0) {
			return fail(StatusBudget, fmt.Errorf("cost estimate $%.4f reached the $%.4f limit after turn %d", res.CostUSD, task.MaxCostUSD, res.Turns))
		}
		res.Turns = turn

		var msg *apiResponse
		var err error
		restoredResponse := durable != nil && len(durable.checkpoint.Response) != 0
		if restoredResponse {
			msg, err = durable.response()
			if err != nil {
				return durableFailure(err)
			}
		} else {
			if durable != nil {
				if err := ctx.Err(); err != nil {
					return fail(stopStatus(ctx), err)
				}
				durable.checkpoint.Pending = &PendingOperation{Kind: "model", ID: fmt.Sprintf("turn-%d", turn)}
				if err := durable.persist(ctx, "model_intent", req, res); err != nil {
					return durableFailure(err)
				}
			}
			msg, err = client.stream(ctx, req, onDelta, onBlock)
		}
		if err != nil && durable == nil && dropContextEdits(&req, err) {
			// A beta hrn sends by default must never make every run fail.
			emit(Event{Time: time.Now(), Kind: EventInfo, Text: "context management refused by the API; retrying without it"})
			msg, err = client.stream(ctx, req, onDelta, onBlock)
		}
		if msg != nil && !restoredResponse {
			turn := Result{InputTokens: msg.Usage.InputTokens, OutputTokens: msg.Usage.OutputTokens,
				CacheRead: msg.Usage.CacheReadInputTokens, CacheWrite: msg.Usage.CacheCreationInputTokens}
			res.ContextTokens = turn.InputTokens + turn.CacheRead + turn.CacheWrite
			emit(Event{Time: time.Now(), Kind: EventUsage, Usage: &Usage{Model: servedModel(msg.Model, model),
				InputTokens: turn.InputTokens, OutputTokens: turn.OutputTokens,
				CacheRead: turn.CacheRead, CacheWrite: turn.CacheWrite, ContextTokens: res.ContextTokens}})
			res.InputTokens += turn.InputTokens
			res.OutputTokens += turn.OutputTokens
			res.CacheRead += turn.CacheRead
			res.CacheWrite += turn.CacheWrite
			// Each turn at the price of the model that served it: a server-side
			// fallback can answer a refused turn with another model.
			res.CostUSD += estimateCost(servedModel(msg.Model, model), turn)
		}
		if err != nil && durable != nil {
			return durableFailure(err)
		}
		if err != nil {
			if ctx.Err() != nil {
				return fail(stopStatus(ctx), ctx.Err())
			}
			return fail(StatusError, err)
		}
		if msg.Model != "" {
			res.Model = msg.Model
			res.ModelReported = true
		}
		if present(msg.ContextManagement) {
			// Verbatim, so a comparison of two runs can attribute a token
			// difference to what the server cleared.
			var compact bytes.Buffer
			if json.Compact(&compact, msg.ContextManagement) != nil {
				compact.Reset()
				compact.Write(msg.ContextManagement)
			}
			emit(Event{Time: time.Now(), Kind: EventInfo, Text: "server context management, turn " + strconv.Itoa(turn) + ": " + compact.String()})
		}
		// History is append-only: the assistant turn goes back exactly as received.
		if !restoredResponse {
			req.Messages = append(req.Messages, apiMessage{Role: "assistant", Content: msg.Content})
			if durable != nil {
				data, err := json.Marshal(msg)
				if err != nil {
					return durableFailure(err)
				}
				durable.checkpoint.Response = data
				durable.checkpoint.InvalidInput = msg.invalidInput
				durable.checkpoint.Pending = nil
				if err := durable.persist(ctx, "model_response", req, res); err != nil {
					return durableFailure(err)
				}
			}
		}

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
			if durable != nil {
				durable.checkpoint.Response = nil
				durable.checkpoint.InvalidInput = nil
				durable.checkpoint.ToolResults = nil
				durable.checkpoint.NextTurn = turn + 1
				if err := durable.persist(ctx, "turn_complete", req, res); err != nil {
					return durableFailure(err)
				}
			}
			continue
		case "tool_use":
			var results []block
			if durable == nil {
				results = b.runTools(ctx, ws, task, calls, msg.invalidInput, byName, emit)
			} else {
				results, err = durable.runTools(ctx, ws, task, calls, msg.invalidInput, byName, emit, req, &res)
				if err != nil {
					return durableFailure(err)
				}
			}
			req.Messages = append(req.Messages, apiMessage{Role: "user", Content: results})
		default:
			res.Status = StatusOK
			res.Final = strings.TrimSpace(text.String())
			if durable != nil {
				durable.checkpoint.Finished = true
				if err := durable.persist(ctx, "finished", req, res); err != nil {
					return durableFailure(err)
				}
				return durable.checkpoint.Result
			}
			saveConversation()
			return finish(&res, started)
		}
		if durable != nil {
			durable.checkpoint.Response = nil
			durable.checkpoint.InvalidInput = nil
			durable.checkpoint.ToolResults = nil
			durable.checkpoint.NextTurn = turn + 1
			if err := durable.persist(ctx, "turn_complete", req, res); err != nil {
				return durableFailure(err)
			}
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
		case refusalOf(t, ws, input) != "":
			// Asked before the approval below: prompting for a command the
			// harness will refuse anyway only wastes the operator's yes.
			result(i, name, tools.GuardError(refusalOf(t, ws, input)).Error(), true)
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

// refusalOf asks a tool whether it declines this call outright.
func refusalOf(t tools.Tool, ws *tools.Workspace, input json.RawMessage) string {
	if t.Refuse == nil {
		return ""
	}
	return t.Refuse(ws, input)
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

// resumeMessages starts the history: an earlier conversation when the task
// continues one, then this task's prompt as the new user turn.
func resumeMessages(task Task) []apiMessage {
	var msgs []apiMessage
	if task.Resume != nil {
		msgs = append(msgs, task.Resume.Messages...)
	}
	return append(msgs, apiMessage{Role: "user", Content: []block{{"type": "text", "text": task.Prompt}}})
}

// resumable drops a trailing assistant turn whose tool calls were never
// answered: the API refuses a history that ends on an unanswered call.
func resumable(msgs []apiMessage) []apiMessage {
	if n := len(msgs); n > 0 && msgs[n-1].Role == "assistant" {
		for _, bl := range msgs[n-1].Content {
			if bl["type"] == "tool_use" {
				return msgs[:n-1]
			}
		}
	}
	return msgs
}

// dropContextEdits removes the context-management request when the API is the
// one refusing it, so the run continues instead of failing on every turn.
func dropContextEdits(req *apiRequest, err error) bool {
	var apiErr *apiError
	if req.ContextManagement == nil || !errors.As(err, &apiErr) || apiErr.Status != 400 {
		return false
	}
	if !strings.Contains(apiErr.Message, "context_management") && !strings.Contains(apiErr.Message, betaContextEdits) {
		return false
	}
	req.ContextManagement = nil
	kept := req.betas[:0]
	for _, b := range req.betas {
		if b != betaContextEdits {
			kept = append(kept, b)
		}
	}
	req.betas = kept
	return true
}

func supportsDefaultFallbacks(model string) bool {
	return model == "claude-opus-5" || model == "claude-fable-5-1"
}

// price is USD per million tokens. The API returns token counts, never money,
// so every cost hrn shows for the api backend comes from this table.
type price struct {
	in, out float64
	// cacheRead is set where it is not the usual 0.1x input: the newest
	// models price cache reads on their own. 5-minute cache writes are 1.25x
	// input on every model.
	cacheRead float64
}

// prices are Anthropic's first-party list prices, as published on 2026-06-24.
// Bedrock and Vertex price separately; their costs here are estimates at best.
var prices = map[string]price{
	"claude-fable-5-1":  {in: 10, out: 50, cacheRead: 0.25},
	"claude-mythos-5-1": {in: 10, out: 50},
	"claude-fable-5":    {in: 10, out: 50},
	"claude-opus-5-5":   {in: 4, out: 20, cacheRead: 0.20},
	"claude-opus-5":     {in: 5, out: 25},
	"claude-opus-4-8":   {in: 5, out: 25},
	"claude-opus-4-7":   {in: 5, out: 25},
	"claude-opus-4-6":   {in: 5, out: 25},
	"claude-sonnet-5":   {in: 2, out: 10},
	"claude-sonnet-4-6": {in: 3, out: 15},
	"claude-haiku-4-5":  {in: 1, out: 5},
}

// datedSuffix is a snapshot date on a model id: -20251001, or @20251001 on
// Vertex. The snapshot bills as its model.
var datedSuffix = regexp.MustCompile(`[-@]\d{8}$`)

// priceOf finds a model's price, looking a dated snapshot id up by its model.
func priceOf(model string) (price, bool) {
	if p, ok := prices[model]; ok {
		return p, true
	}
	p, ok := prices[datedSuffix.ReplaceAllString(model, "")]
	return p, ok
}

// servedModel is the model to price a turn at: the one the response names when
// the table knows it, otherwise the one the run asked for.
func servedModel(reported, requested string) string {
	if _, ok := priceOf(reported); ok {
		return reported
	}
	return requested
}

func estimateCost(model string, r Result) float64 {
	p, ok := priceOf(model)
	if !ok {
		return 0
	}
	cacheRead := p.cacheRead
	if cacheRead == 0 {
		cacheRead = 0.1 * p.in
	}
	return (float64(r.InputTokens)*p.in + float64(r.CacheRead)*cacheRead +
		1.25*float64(r.CacheWrite)*p.in + float64(r.OutputTokens)*p.out) / 1e6
}
