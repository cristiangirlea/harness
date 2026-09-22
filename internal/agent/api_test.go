package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAPI replays scripted SSE turns and records each request body and headers.
// A turn of nil answers 529 overloaded without consuming a scripted turn.
type fakeAPI struct {
	mu       sync.Mutex
	turns    [][]string
	requests []map[string]any
	headers  []http.Header
	served   int
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	f.mu.Lock()
	i := f.served
	f.served++
	f.requests = append(f.requests, req)
	f.headers = append(f.headers, r.Header.Clone())
	f.mu.Unlock()
	if i < len(f.turns) && f.turns[i] == nil {
		w.Header().Set("retry-after", "0.01")
		http.Error(w, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`, 529)
		return
	}
	if i >= len(f.turns) {
		http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"no more turns"}}`, 400)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, ev := range f.turns[i] {
		var typ struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(ev), &typ)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ.Type, ev)
	}
}

func messageStart(id string) string {
	return `{"type":"message_start","message":{"id":"` + id + `","type":"message","role":"assistant","model":"claude-opus-5","content":[],"stop_reason":null,"usage":{"input_tokens":100,"output_tokens":1}}}`
}

func messageEnd(stop string) []string {
	return []string{
		`{"type":"message_delta","delta":{"stop_reason":"` + stop + `","stop_sequence":null,"stop_details":null},"usage":{"output_tokens":20}}`,
		`{"type":"message_stop"}`,
	}
}

func toolTurn(id, name, input string) []string {
	enc, _ := json.Marshal(input)
	return append([]string{
		messageStart("msg_" + id),
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"` + id + `","name":"` + name + `","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":` + string(enc) + `}}`,
		`{"type":"content_block_stop","index":0}`,
	}, messageEnd("tool_use")...)
}

func textTurn(text string) []string {
	enc, _ := json.Marshal(text)
	return append([]string{
		messageStart("msg_final"),
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":` + string(enc) + `}}`,
		`{"type":"content_block_stop","index":0}`,
	}, messageEnd("end_turn")...)
}

func runFake(t *testing.T, turns [][]string, task Task) (Result, []Event, *fakeAPI) {
	t.Helper()
	fake := &fakeAPI{turns: turns}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	b := NewAPIBackend()
	b.BaseURL, b.APIKey = srv.URL, "test"
	var events []Event
	var mu sync.Mutex
	res := b.Run(context.Background(), task, func(e Event) {
		mu.Lock()
		defer mu.Unlock()
		if !e.Partial {
			events = append(events, e)
		}
	})
	return res, events, fake
}

// lastToolResult pulls the tool_result block out of the final request.
func lastToolResult(t *testing.T, req map[string]any) map[string]any {
	t.Helper()
	msgs := req["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	block := last["content"].([]any)[0].(map[string]any)
	if block["type"] != "tool_result" {
		t.Fatalf("expected tool_result, got %v", block)
	}
	return block
}

func TestAPILoopRunsToolsAndFinishes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("secret-value\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, events, fake := runFake(t, [][]string{
		toolTurn("toolu_1", "read_file", `{"path":"hello.txt"}`),
		textTurn("The file says secret-value."),
	}, Task{Prompt: "what is in hello.txt?", Dir: dir, Mode: ModeAsk})

	if res.Status != StatusOK || res.Final != "The file says secret-value." || res.Turns != 2 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.InputTokens != 200 || res.OutputTokens != 40 {
		t.Errorf("usage not accumulated: in=%d out=%d", res.InputTokens, res.OutputTokens)
	}

	block := lastToolResult(t, fake.requests[1])
	if block["tool_use_id"] != "toolu_1" || !strings.Contains(fmt.Sprint(block["content"]), "secret-value") {
		t.Errorf("tool_result not sent back correctly: %v", block)
	}

	// First request must carry the harness defaults.
	first := fake.requests[0]
	if first["model"] != "claude-opus-5" || first["fallbacks"] != "default" {
		t.Errorf("model/fallbacks not set: model=%v fallbacks=%v", first["model"], first["fallbacks"])
	}
	if th, _ := first["thinking"].(map[string]any); th["type"] != "adaptive" {
		t.Errorf("thinking not adaptive: %v", first["thinking"])
	}

	var kinds []EventKind
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	want := []EventKind{EventToolCall, EventToolResult, EventText}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Errorf("events = %v, want %v", kinds, want)
	}
}

func TestAPILoopDeniesWritesWithoutApproval(t *testing.T) {
	dir := t.TempDir()
	res, _, fake := runFake(t, [][]string{
		toolTurn("toolu_w", "write_file", `{"path":"x.txt","content":"hi"}`),
		textTurn("I was not allowed to write."),
	}, Task{Prompt: "write x.txt", Dir: dir, Mode: ModeAsk})

	if res.Status != StatusOK {
		t.Fatalf("unexpected result: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "x.txt")); err == nil {
		t.Fatal("file was written despite no approval")
	}
	if block := lastToolResult(t, fake.requests[1]); block["is_error"] != true {
		t.Errorf("denied call should be an error result: %v", block)
	}
}

func TestAPILoopGuardsCommands(t *testing.T) {
	dir := t.TempDir()
	_, _, fake := runFake(t, [][]string{
		toolTurn("toolu_g", "run_command", `{"command":"sudo rm -rf /"}`),
		textTurn("I was blocked."),
	}, Task{Prompt: "clean up", Dir: dir, Mode: ModeYolo})
	block := lastToolResult(t, fake.requests[1])
	if block["is_error"] != true || !strings.Contains(fmt.Sprint(block["content"]), "command guard (sudo)") {
		t.Errorf("a guarded command should come back as an error result: %v", block)
	}

	// With the operator's consent the same line reaches the shell.
	_, _, fake = runFake(t, [][]string{
		toolTurn("toolu_a", "run_command", `{"command":"echo \"git push\""}`),
		textTurn("done"),
	}, Task{Prompt: "echo it", Dir: dir, Mode: ModeYolo, AllowDangerous: true})
	block = lastToolResult(t, fake.requests[1])
	if block["is_error"] == true || !strings.Contains(fmt.Sprint(block["content"]), "git push") {
		t.Errorf("allow_dangerous should let the command run: %v", block)
	}
}

func TestAPILoopSaysWhetherTheGuardIsOn(t *testing.T) {
	for _, tc := range []struct {
		allow bool
		want  string
	}{
		{false, "Command guard: on"},
		{true, "Command guard: off"},
	} {
		_, _, fake := runFake(t, [][]string{textTurn("ok")},
			Task{Prompt: "go", Dir: t.TempDir(), Mode: ModeYolo, AllowDangerous: tc.allow})
		sys := systemText(t, fake.requests[0])
		if env := sys[len(sys)-1]; !strings.Contains(env, tc.want) {
			t.Errorf("allow_dangerous=%v: environment block is missing %q:\n%s", tc.allow, tc.want, env)
		}
	}
}

func TestAPILoopReadOnlyHidesWriteTools(t *testing.T) {
	_, _, fake := runFake(t, [][]string{textTurn("ok")}, Task{Prompt: "look", Dir: t.TempDir(), Mode: ModeReadOnly})
	for _, tl := range fake.requests[0]["tools"].([]any) {
		name := tl.(map[string]any)["name"]
		if name == "write_file" || name == "edit_file" || name == "run_command" {
			t.Errorf("readonly run offered %v", name)
		}
	}
}

func TestAPILoopMaxTurns(t *testing.T) {
	dir := t.TempDir()
	loop := toolTurn("toolu_l", "list_files", `{}`)
	res, _, _ := runFake(t, [][]string{loop, loop, loop}, Task{Prompt: "loop", Dir: dir, Mode: ModeAsk, MaxTurns: 2})
	if res.Status != StatusMaxTurns {
		t.Fatalf("status = %s, want max_turns", res.Status)
	}
}

func TestAPIRetriesOverloadedThenSucceeds(t *testing.T) {
	res, _, fake := runFake(t, [][]string{nil, textTurn("done")}, Task{Prompt: "hi", Dir: t.TempDir(), Mode: ModeAsk})
	if res.Status != StatusOK || res.Final != "done" || len(fake.requests) != 2 {
		t.Fatalf("status=%s final=%q requests=%d err=%s", res.Status, res.Final, len(fake.requests), res.Error)
	}
	h := fake.headers[0]
	if h.Get("x-api-key") != "test" || h.Get("anthropic-version") != apiVersion || !strings.Contains(h.Get("anthropic-beta"), betaFallback) {
		t.Errorf("headers wrong: %v", h)
	}
}

func TestAPIEchoesThinkingSignature(t *testing.T) {
	dir := t.TempDir()
	turn := append([]string{
		messageStart("msg_th"),
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"plan"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig123"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_t","name":"list_files","input":{}}}`,
		`{"type":"content_block_stop","index":1}`,
	}, messageEnd("tool_use")...)
	_, _, fake := runFake(t, [][]string{turn, textTurn("ok")}, Task{Prompt: "go", Dir: dir, Mode: ModeAsk})

	msgs := fake.requests[1]["messages"].([]any)
	assistant := msgs[1].(map[string]any)["content"].([]any)
	th := assistant[0].(map[string]any)
	tu := assistant[1].(map[string]any)
	if th["thinking"] != "plan" || th["signature"] != "sig123" {
		t.Errorf("thinking block not echoed intact: %v", th)
	}
	if in, ok := tu["input"].(map[string]any); !ok || len(in) != 0 {
		t.Errorf("empty tool input should echo as {}: %v", tu["input"])
	}
}

func TestAPIRejectsInvalidToolJSON(t *testing.T) {
	_, _, fake := runFake(t, [][]string{
		append([]string{
			messageStart("msg_bad"),
			`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_bad","name":"read_file","input":{}}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\": \"a"}}`,
			`{"type":"content_block_stop","index":0}`,
		}, messageEnd("tool_use")...),
		textTurn("ok"),
	}, Task{Prompt: "go", Dir: t.TempDir(), Mode: ModeAsk})
	block := lastToolResult(t, fake.requests[1])
	if block["is_error"] != true || !strings.Contains(fmt.Sprint(block["content"]), "not valid JSON") {
		t.Errorf("invalid input should be reported: %v", block)
	}
}

// systemText returns the text of each system block of a request.
func systemText(t *testing.T, req map[string]any) []string {
	t.Helper()
	var out []string
	for _, b := range req["system"].([]any) {
		out = append(out, b.(map[string]any)["text"].(string))
	}
	return out
}

func TestAPILoopSendsProjectInstructions(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "CLAUDE.md", "Always run gofmt before finishing.")
	writeFile(t, dir, "AGENTS.md", "Prefer table-driven tests.")

	_, events, fake := runFake(t, [][]string{textTurn("ok")}, Task{Prompt: "go", Dir: dir, Mode: ModeAsk})
	sys := systemText(t, fake.requests[0])
	if len(sys) != 3 {
		t.Fatalf("want system = [prompt, instructions, env], got %d blocks", len(sys))
	}
	if !strings.Contains(sys[1], "## AGENTS.md") || !strings.Contains(sys[1], "Prefer table-driven tests.") ||
		!strings.Contains(sys[1], "## CLAUDE.md") || !strings.Contains(sys[1], "Always run gofmt before finishing.") {
		t.Errorf("instructions block = %q", sys[1])
	}
	if strings.Index(sys[1], "AGENTS.md") > strings.Index(sys[1], "CLAUDE.md") {
		t.Error("files must appear in a fixed order")
	}
	// The cache breakpoint stays on the last block, so tools, prompt and
	// instructions are one cached prefix.
	blocks := fake.requests[0]["system"].([]any)
	if _, cached := blocks[1].(map[string]any)["cache_control"]; cached {
		t.Error("the instructions block must not carry the breakpoint")
	}
	if _, cached := blocks[2].(map[string]any)["cache_control"]; !cached {
		t.Error("the env block keeps the breakpoint")
	}

	var infos []string
	for _, e := range events {
		if e.Kind == EventInfo {
			infos = append(infos, e.Text)
		}
	}
	if len(infos) != 1 || !strings.HasPrefix(infos[0], "instructions: AGENTS.md, CLAUDE.md (") {
		t.Errorf("info events = %v", infos)
	}
}

func TestAPILoopWithoutProjectInstructions(t *testing.T) {
	dir := t.TempDir()
	_, _, fake := runFake(t, [][]string{textTurn("ok")}, Task{Prompt: "go", Dir: dir, Mode: ModeAsk})
	if sys := systemText(t, fake.requests[0]); len(sys) != 2 {
		t.Errorf("no instruction file: want 2 system blocks, got %d", len(sys))
	}

	writeFile(t, dir, "CLAUDE.md", "Always run gofmt.")
	_, events, fake := runFake(t, [][]string{textTurn("ok")}, Task{Prompt: "go", Dir: dir, Mode: ModeAsk, NoInstructions: true})
	if sys := systemText(t, fake.requests[0]); len(sys) != 2 {
		t.Errorf("NoInstructions: want 2 system blocks, got %d", len(sys))
	}
	for _, e := range events {
		if e.Kind == EventInfo && strings.HasPrefix(e.Text, "instructions:") {
			t.Error("NoInstructions must not announce instructions")
		}
	}
}

func TestAPILoopReportsUnreadableInstructions(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "CLAUDE.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, events, fake := runFake(t, [][]string{textTurn("ok")}, Task{Prompt: "go", Dir: dir, Mode: ModeAsk})
	if res.Status != StatusOK {
		t.Fatalf("an unreadable instruction file must not fail the run: %+v", res)
	}
	if sys := systemText(t, fake.requests[0]); len(sys) != 2 {
		t.Errorf("want 2 system blocks, got %d", len(sys))
	}
	var errs int
	for _, e := range events {
		if e.Kind == EventError && strings.Contains(e.Text, "project instructions") {
			errs++
		}
	}
	if errs != 1 {
		t.Errorf("the failure should be reported once, got %d error events", errs)
	}
}

func TestAPIStopsOnTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	t.Cleanup(srv.Close)
	b := NewAPIBackend()
	b.BaseURL, b.APIKey, b.MaxRetries = srv.URL, "test", 0

	started := time.Now()
	res := b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Mode: ModeAsk, Timeout: 200 * time.Millisecond}, func(Event) {})
	if res.Status != StatusTimeout {
		t.Fatalf("status = %s, want timeout (error %q)", res.Status, res.Error)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("took %s; the deadline should have cut it short", elapsed)
	}
}

func TestAPIStopsOnCostBudget(t *testing.T) {
	// One turn costs more than the limit, so the second turn never starts.
	res, _, fake := runFake(t, [][]string{
		toolTurn("toolu_1", "list_files", `{}`),
		textTurn("never reached"),
	}, Task{Prompt: "go", Dir: t.TempDir(), Mode: ModeAsk, MaxCostUSD: 0.0001})
	if res.Status != StatusBudget {
		t.Fatalf("status = %s, want budget (error %q)", res.Status, res.Error)
	}
	if !strings.Contains(res.Error, "$0.0001") || !strings.Contains(res.Error, "turn 1") {
		t.Errorf("error should name the limit and the turn: %q", res.Error)
	}
	if len(fake.requests) != 1 {
		t.Errorf("requests = %d, want 1: the run stops before spending more", len(fake.requests))
	}
	if res.CostUSD == 0 {
		t.Error("the cost so far should still be reported")
	}
}

func TestAPISendsTaskBudgetWithEffort(t *testing.T) {
	_, _, fake := runFake(t, [][]string{textTurn("ok")},
		Task{Prompt: "go", Dir: t.TempDir(), Mode: ModeAsk, Effort: "high", TaskBudget: 64000})
	out, ok := fake.requests[0]["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("no output_config: %v", fake.requests[0])
	}
	if out["effort"] != "high" {
		t.Errorf("task_budget must not overwrite effort: %v", out)
	}
	tb, ok := out["task_budget"].(map[string]any)
	if !ok || tb["type"] != "tokens" || tb["total"] != float64(64000) {
		t.Fatalf("task_budget = %v", out["task_budget"])
	}
	if _, present := tb["remaining"]; present {
		t.Error("remaining must stay unset: hx resends the whole history, so the server tracks the countdown")
	}
	if h := fake.headers[0].Get("anthropic-beta"); !strings.Contains(h, betaTaskBudget) {
		t.Errorf("anthropic-beta = %q, want it to carry %s", h, betaTaskBudget)
	}

	_, _, fake = runFake(t, [][]string{textTurn("ok")}, Task{Prompt: "go", Dir: t.TempDir(), Mode: ModeAsk})
	if _, present := fake.requests[0]["output_config"]; present {
		t.Errorf("no budget and no effort should mean no output_config: %v", fake.requests[0]["output_config"])
	}
	if h := fake.headers[0].Get("anthropic-beta"); strings.Contains(h, betaTaskBudget) {
		t.Errorf("anthropic-beta = %q, want no task budget beta", h)
	}
}

func TestValidateLimits(t *testing.T) {
	if err := ValidateLimits(0, 0, 0); err != nil {
		t.Errorf("unset is fine: %v", err)
	}
	if err := ValidateLimits(time.Minute, 2.5, 20000); err != nil {
		t.Errorf("the minimum budget and positive limits are allowed: %v", err)
	}
	for _, tc := range []struct {
		name       string
		timeout    time.Duration
		maxCost    float64
		taskBudget int
		want       string
	}{
		{"small budget", 0, 0, 100, "20000"},
		{"negative timeout", -time.Minute, 0, 0, "negative"},
		{"negative cost", 0, -1, 0, "negative"},
	} {
		err := ValidateLimits(tc.timeout, tc.maxCost, tc.taskBudget)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

func TestAPISaysWhenCostCannotBeEnforced(t *testing.T) {
	var infos []string
	b := NewAPIBackend()
	b.APIKey, b.MaxRetries = "test", 0
	b.BaseURL = "http://127.0.0.1:1" // never reached: the notice comes first
	collect := func(e Event) {
		if e.Kind == EventInfo {
			infos = append(infos, e.Text)
		}
	}
	b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Mode: ModeAsk,
		Model: "claude-not-in-the-price-table", MaxCostUSD: 1}, collect)
	if len(infos) != 1 || !strings.Contains(infos[0], "max_cost cannot be enforced") {
		t.Fatalf("info events = %v", infos)
	}

	infos = nil
	b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Mode: ModeAsk,
		Model: "claude-opus-5", MaxCostUSD: 1}, collect)
	for _, text := range infos {
		if strings.Contains(text, "max_cost cannot be enforced") {
			t.Errorf("a priced model should not warn: %q", text)
		}
	}
}

func TestAPISendsContextManagementByDefault(t *testing.T) {
	_, _, fake := runFake(t, [][]string{textTurn("ok")}, Task{Prompt: "go", Dir: t.TempDir(), Mode: ModeAsk})
	cm, ok := fake.requests[0]["context_management"].(map[string]any)
	if !ok {
		t.Fatalf("no context_management: %v", fake.requests[0])
	}
	edits, ok := cm["edits"].([]any)
	if !ok || len(edits) != 1 {
		t.Fatalf("edits = %v", cm["edits"])
	}
	if edits[0].(map[string]any)["type"] != "clear_tool_uses_20250919" {
		t.Errorf("edit = %v", edits[0])
	}
	// The header is a comma-joined list: the fallback beta must survive.
	h := fake.headers[0].Get("anthropic-beta")
	if !strings.Contains(h, betaContextEdits) || !strings.Contains(h, betaFallback) {
		t.Errorf("anthropic-beta = %q, want both betas", h)
	}

	_, _, fake = runFake(t, [][]string{textTurn("ok")}, Task{Prompt: "go", Dir: t.TempDir(), Mode: ModeAsk, NoContextEdits: true})
	if _, present := fake.requests[0]["context_management"]; present {
		t.Error("NoContextEdits must drop the field")
	}
	if h := fake.headers[0].Get("anthropic-beta"); strings.Contains(h, betaContextEdits) {
		t.Errorf("anthropic-beta = %q, want no context beta", h)
	}
}

func TestAPIRetriesWithoutContextManagementWhenRefused(t *testing.T) {
	var reqs []map[string]any
	var betas []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		reqs = append(reqs, req)
		betas = append(betas, r.Header.Get("anthropic-beta"))
		if _, sent := req["context_management"]; sent {
			http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"context_management is not supported with this beta"}}`, 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range textTurn("recovered") {
			var typ struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal([]byte(ev), &typ)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ.Type, ev)
		}
	}))
	t.Cleanup(srv.Close)

	b := NewAPIBackend()
	b.BaseURL, b.APIKey, b.MaxRetries = srv.URL, "test", 0
	var infos []string
	res := b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Mode: ModeAsk}, func(e Event) {
		if e.Kind == EventInfo {
			infos = append(infos, e.Text)
		}
	})
	if res.Status != StatusOK || res.Final != "recovered" {
		t.Fatalf("the run should survive a refused beta: %+v", res)
	}
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want the refused one and one retry", len(reqs))
	}
	if _, sent := reqs[1]["context_management"]; sent {
		t.Error("the retry must not send the field again")
	}
	if strings.Contains(betas[1], betaContextEdits) {
		t.Errorf("the retry must drop the beta too: %q", betas[1])
	}
	var said bool
	for _, text := range infos {
		if strings.Contains(text, "context management refused") {
			said = true
		}
	}
	if !said {
		t.Errorf("the run should say why: %v", infos)
	}
}

func TestAPISavesAndResumesTheConversation(t *testing.T) {
	dir := t.TempDir()
	runDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, _, fake := runFake(t, [][]string{
		toolTurn("toolu_1", "read_file", `{"path":"hello.txt"}`),
		textTurn("It says first."),
	}, Task{Prompt: "what is in hello.txt?", Dir: dir, Mode: ModeAsk, RunDir: runDir})
	if res.Status != StatusOK {
		t.Fatalf("res = %+v", res)
	}

	data, err := os.ReadFile(filepath.Join(runDir, "messages.json"))
	if err != nil {
		t.Fatalf("no saved conversation: %v", err)
	}
	var saved Conversation
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Model != res.Model {
		t.Errorf("model = %q, want %q", saved.Model, res.Model)
	}
	if len(saved.Messages) != 4 {
		t.Fatalf("messages = %d, want user, assistant, user(tool_result), assistant", len(saved.Messages))
	}
	for i, want := range []string{"user", "assistant", "user", "assistant"} {
		if saved.Messages[i].Role != want {
			t.Errorf("message %d role = %q, want %q", i, saved.Messages[i].Role, want)
		}
	}

	// Resuming sends the saved exchange plus the follow-up, in order.
	_, _, fake2 := runFake(t, [][]string{textTurn("Still first.")},
		Task{Prompt: "and now?", Dir: dir, Mode: ModeAsk, Resume: &saved})
	msgs := fake2.requests[0]["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("resumed request has %d messages, want 5", len(msgs))
	}
	last := msgs[4].(map[string]any)
	if last["role"] != "user" {
		t.Errorf("the follow-up must come last: %v", last)
	}
	if text := fmt.Sprint(last["content"]); !strings.Contains(text, "and now?") {
		t.Errorf("follow-up content = %v", last["content"])
	}
	// The assistant turn goes back exactly as the first run received it.
	before := fake.requests[1]["messages"].([]any)[1]
	if fmt.Sprint(msgs[1]) != fmt.Sprint(before) {
		t.Errorf("assistant turn changed on resume:\n got %v\nwant %v", msgs[1], before)
	}
}

func TestAPIDoesNotSaveADanglingToolCall(t *testing.T) {
	runDir := t.TempDir()
	// The turn asks for a tool and then hits max_tokens, so its calls are
	// never answered; resuming from that would be rejected by the API.
	turn := append([]string{
		messageStart("msg_trunc"),
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_x","name":"list_files","input":{}}}`,
		`{"type":"content_block_stop","index":0}`,
	}, messageEnd("max_tokens")...)
	res, _, _ := runFake(t, [][]string{turn}, Task{Prompt: "go", Dir: t.TempDir(), Mode: ModeAsk, RunDir: runDir})
	if res.Status != StatusError {
		t.Fatalf("res = %+v", res)
	}
	data, err := os.ReadFile(filepath.Join(runDir, "messages.json"))
	if err != nil {
		t.Fatalf("a failed run should still save what can be resumed: %v", err)
	}
	var saved Conversation
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Messages) != 1 || saved.Messages[0].Role != "user" {
		t.Fatalf("the unanswered assistant turn must be dropped: %+v", saved.Messages)
	}
}

func TestAPISavesNothingWhenTheRunNeverStarted(t *testing.T) {
	runDir := t.TempDir()
	b := NewAPIBackend()
	b.APIKey, b.MaxRetries = "test", 0
	// The workspace is a file, so the run fails before it builds a request.
	notADir := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := b.Run(context.Background(), Task{Prompt: "go", Dir: notADir, Mode: ModeAsk, RunDir: runDir}, func(Event) {})
	if res.Status != StatusError {
		t.Fatalf("res = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(runDir, "messages.json")); err == nil {
		t.Fatal("a run with no conversation must not leave one behind: continuing it would start from nothing")
	}
}
