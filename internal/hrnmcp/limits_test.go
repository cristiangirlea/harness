package hrnmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/followup"
	"github.com/cristiangirlea/harness/internal/limits"
	"github.com/cristiangirlea/harness/internal/mcp"
	"github.com/cristiangirlea/harness/internal/prefs"
	"github.com/cristiangirlea/harness/internal/resolve"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// scripted is a backend whose answer a test chooses.
type scripted struct {
	name string
	run  func(agent.Task) agent.Result
}

func (s scripted) Name() string            { return s.name }
func (s scripted) Detect() (string, error) { return "built-in stub", nil }
func (s scripted) Run(_ context.Context, task agent.Task, _ func(agent.Event)) agent.Result {
	res := s.run(task)
	res.Backend = s.name
	return res
}

// standIn puts scripted backends in place of claude and codex for one test,
// so no real CLI can run. codex reaches its limit; claude finishes.
func standIn(t *testing.T) {
	t.Helper()
	// A key in the environment would keep the partner from standing in.
	for _, name := range append(followup.MeteredEnv("claude"), followup.MeteredEnv("codex")...) {
		t.Setenv(name, "")
	}
	for _, s := range []scripted{
		{"codex", func(agent.Task) agent.Result {
			return agent.Result{Status: agent.StatusLimited, Error: "You've hit your usage limit.", LimitResets: time.Now().Add(time.Hour)}
		}},
		{"claude", func(agent.Task) agent.Result {
			return agent.Result{Status: agent.StatusOK, Final: "finished it", Turns: 1}
		}},
	} {
		orig, err := agent.Get(s.name)
		if err != nil {
			t.Fatal(err)
		}
		agent.Register(s)
		t.Cleanup(func() { agent.Register(orig) })
	}
}

func TestRunToolCarriesALimitedRunOn(t *testing.T) {
	s := connect(t, t.TempDir())
	standIn(t)
	res := s.call(t, "run", `{"prompt":"fix it","purpose":"review","backend":"codex","mode":"auto"}`)
	if res.IsError {
		t.Fatalf("the carried-on run finished, yet: %s", res.Text())
	}
	text := res.Text()
	if !strings.Contains(text, "finished it") || !strings.Contains(text, "codex reached its usage limit") {
		t.Errorf("text = %q, want claude's answer and why it ran", text)
	}
	sc := res.StructuredContent.(map[string]any)
	id, _ := sc["run_id"].(string)
	rec, err := runlog.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Backend != "claude" || rec.Continues == "" {
		t.Fatalf("run_id names %s continuing %q, want the claude run that carried it on", rec.Backend, rec.Continues)
	}
	prev, err := runlog.Load(rec.Continues)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []runlog.Record{rec, prev} {
		if want := agent.ResolveSetup(r.Backend, r.Task); !reflect.DeepEqual(r.Setup, want) {
			t.Errorf("setup for %s = %+v, want %+v", r.Backend, r.Setup, want)
		}
	}
	if rec.Task.Purpose != "review" {
		t.Errorf("carried-on purpose = %q, want review", rec.Task.Purpose)
	}
	if limited, _ := sc["limited_run_id"].(string); limited != rec.Continues {
		t.Errorf("limited_run_id = %q, want %q", limited, rec.Continues)
	}
	if prev, _ := runlog.Load(rec.Continues); prev.Backend != "codex" || prev.Result.Status != agent.StatusLimited {
		t.Errorf("the run it continues: %s %s", prev.Backend, prev.Result.Status)
	}
}

func TestRunToolCarryOnTurnCapAndNote(t *testing.T) {
	for _, from := range []string{"claude", "codex"} {
		t.Run(from, func(t *testing.T) {
			s := connect(t, t.TempDir())
			standIn(t)
			to, want := "claude", 30
			if from == "claude" {
				to, want = "codex", 0
			}
			got := -99
			agent.Register(scripted{from, func(agent.Task) agent.Result {
				return agent.Result{Status: agent.StatusLimited, Error: "You've hit your usage limit.", LimitResets: time.Now().Add(time.Hour)}
			}})
			agent.Register(scripted{to, func(task agent.Task) agent.Result {
				got = task.MaxTurns
				return agent.Result{Status: agent.StatusOK, Final: "done"}
			}})
			res := s.call(t, "run", fmt.Sprintf(`{"prompt":"work","purpose":"review","backend":%q,"mode":"auto"}`, from))
			if res.IsError || got != want || !strings.Contains(res.Text(), resolve.TurnDefaultInfo) {
				t.Fatalf("result=%s cap=%d want=%d", res.Text(), got, want)
			}
			id, _ := res.StructuredContent.(map[string]any)["run_id"].(string)
			rec, err := runlog.Load(id)
			if err != nil || rec.Backend != to || rec.Task.MaxTurns != want {
				t.Fatalf("record=%+v err=%v", rec, err)
			}
		})
	}
}

func TestRunToolStartsOnTheOtherBackendWhileALimitIsRemembered(t *testing.T) {
	s := connect(t, t.TempDir())
	standIn(t)
	_ = limits.Note("codex", agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)})
	res := s.call(t, "run", `{"prompt":"fix it","backend":"codex","model":"gpt-5.5","mode":"auto"}`)
	if res.IsError {
		t.Fatalf("run: %s", res.Text())
	}
	id, _ := res.StructuredContent.(map[string]any)["run_id"].(string)
	rec, _ := runlog.Load(id)
	if rec.Backend != "claude" || rec.Task.Model != "" {
		t.Errorf("ran on %s with model %q, want claude and no codex model", rec.Backend, rec.Task.Model)
	}
	if rec.RouteNote == "" || strings.Contains(rec.RouteNote, "backend codex was named") {
		t.Errorf("route note %q: a run moved to its partner must say why, not keep the named backend", rec.RouteNote)
	}
}

func TestRunToolExplainsAMeteredPartnerBeforeStartingAndKeepsItsModel(t *testing.T) {
	for _, continuing := range []bool{false, true} {
		t.Run(fmt.Sprint(continuing), func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			standIn(t)
			t.Setenv("ANTHROPIC_API_KEY", "fake-secret-value")
			dir := t.TempDir()
			args := map[string]any{"prompt": "fix it", "backend": "codex", "model": "requested-model"}
			if continuing {
				rec, err := runlog.Create("codex", agent.Task{Prompt: "first", Dir: dir}, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := rec.Close(agent.Result{Backend: "codex", Status: agent.StatusOK}); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(rec.Dir(), "messages.json"), []byte(`{"model":"previous-model"}`), 0o644); err != nil {
					t.Fatal(err)
				}
				args["continue_run_id"] = rec.ID()
			}
			if err := limits.Note("codex", agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			called := false
			agent.Register(scripted{"codex", func(task agent.Task) agent.Result {
				called = true
				if task.Model != "requested-model" {
					t.Errorf("model = %q, want requested-model", task.Model)
				}
				events, err := runlog.Events(filepath.Base(task.RunDir))
				if err != nil {
					t.Fatal(err)
				}
				said := false
				for _, e := range events {
					said = said || e.Kind == agent.EventInfo && strings.Contains(e.Text, "ANTHROPIC_API_KEY") && strings.Contains(e.Text, "per token")
					if strings.Contains(e.Text, "fake-secret-value") {
						t.Error("event exposed the key value")
					}
				}
				if !said {
					t.Error("no metered partner reason before the backend started")
				}
				return agent.Result{Status: agent.StatusOK}
			}})
			raw, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			res, err := runTool(dir, false).Handler(context.Background(), mcp.Call{Arguments: raw, Progress: func(string) {}})
			if err != nil || res.IsError || !called {
				t.Fatalf("result=%+v err=%v called=%v", res, err, called)
			}
		})
	}
}

func TestRunToolDoesNotCreateACarryOnAfterCancellation(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agent.Register(scripted{"codex", func(agent.Task) agent.Result {
		cancel()
		return agent.Result{Status: agent.StatusLimited, Error: "usage limit", LimitResets: time.Now().Add(time.Hour)}
	}})
	res, err := runTool(t.TempDir(), false).Handler(ctx, mcp.Call{Arguments: json.RawMessage(`{"prompt":"fix it","backend":"codex"}`), Progress: func(string) {}})
	if err != nil || !res.IsError || !strings.Contains(res.Text(), "status: limited") {
		t.Errorf("result=%+v err=%v, want the limited run's result", res, err)
	}
	recs, err := runlog.List(5)
	if err != nil || len(recs) != 1 || recs[0].Backend != "codex" || recs[0].Result.Status != agent.StatusLimited {
		t.Fatalf("records = %+v, err=%v; want only the limited run", recs, err)
	}
	if limits.Until("codex", time.Now()).IsZero() {
		t.Error("codex's limit was not remembered in limits.json")
	}
}

func TestRunToolNoSwitchStays(t *testing.T) {
	s := connect(t, t.TempDir())
	standIn(t)
	res := s.call(t, "run", `{"prompt":"fix it","backend":"codex","mode":"auto","no_switch":true}`)
	if !res.IsError || !strings.Contains(res.Text(), "status: limited") {
		t.Errorf("want the limited result itself: %s", res.Text())
	}
	if recs, _ := runlog.List(5); len(recs) != 1 {
		t.Errorf("runs = %d, want only the codex one", len(recs))
	}
}

func TestBackendsToolShowsARememberedLimit(t *testing.T) {
	s := connect(t, t.TempDir())
	standIn(t)
	_ = limits.Note("codex", agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)})
	res := s.call(t, "backends", `{}`)
	if !strings.Contains(res.Text(), "usage limit until") {
		t.Errorf("backends = %q, want codex's remembered limit", res.Text())
	}
	for _, b := range res.StructuredContent.(map[string]any)["backends"].([]any) {
		m := b.(map[string]any)
		if m["name"] == "codex" && m["limited_until"] == nil {
			t.Errorf("codex entry has no limited_until: %v", m)
		}
		if m["name"] == "claude" && m["limited_until"] != nil {
			t.Errorf("claude is not limited: %v", m)
		}
	}
}

func TestRunToolRemembersBothLimits(t *testing.T) {
	s := connect(t, t.TempDir())
	standIn(t)
	orig, _ := agent.Get("claude")
	agent.Register(scripted{"claude", func(agent.Task) agent.Result {
		return agent.Result{Status: agent.StatusLimited, Error: "You've hit your session limit.", LimitResets: time.Now().Add(2 * time.Hour)}
	}})
	t.Cleanup(func() { agent.Register(orig) })

	if res := s.call(t, "run", `{"prompt":"fix it","backend":"codex","mode":"auto"}`); !res.IsError {
		t.Errorf("both ended limited, yet: %s", res.Text())
	}
	for _, name := range []string{"codex", "claude"} {
		if limits.Until(name, time.Now()).IsZero() {
			t.Errorf("%s's limit was not remembered", name)
		}
	}
}

// The answer is the carry-on's, so its record error is the one reported, and
// an unsaved result.json on it makes the call an error as for any run.
func TestRunToolReportsTheCarryOnsRecordError(t *testing.T) {
	s := connect(t, t.TempDir())
	standIn(t)
	orig, _ := agent.Get("claude")
	agent.Register(scripted{"claude", func(task agent.Task) agent.Result {
		if err := os.Mkdir(filepath.Join(task.RunDir, "result.json"), 0o755); err != nil {
			return agent.Result{Status: agent.StatusError, Error: err.Error()}
		}
		return agent.Result{Status: agent.StatusOK, Final: "finished it", Turns: 1}
	}})
	t.Cleanup(func() { agent.Register(orig) })

	res := s.call(t, "run", `{"prompt":"fix it","backend":"codex","mode":"auto"}`)
	if !res.IsError {
		t.Errorf("the carry-on's result.json was not saved, yet no error: %s", res.Text())
	}
	sc := res.StructuredContent.(map[string]any)
	recordError, _ := sc["record_error"].(string)
	if !strings.Contains(recordError, "result.json") || !strings.Contains(res.Text(), "saving run: ") {
		t.Errorf("record_error = %q, text = %q: want the carry-on's record error", recordError, res.Text())
	}
	if sc["limited_run_id"] == nil || sc["run_id"] == sc["limited_run_id"] {
		t.Errorf("structured = %v: want the carry-on's run and the limited one", sc)
	}
	if result := sc["result"].(map[string]any); result["status"] != "ok" || result["backend"] != "claude" {
		t.Errorf("result = %v: want claude's ok result kept", result)
	}
}

func TestLimitCarryOnRecordsContextWarning(t *testing.T) {
	for _, tokens := range []int64{150000, 150001} {
		t.Run(fmt.Sprint(tokens), func(t *testing.T) {
			dir := t.TempDir()
			s := connect(t, dir)
			standIn(t)
			w, err := runlog.Create("claude", agent.Task{Prompt: "first", Dir: dir}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(w.Dir(), "messages.json"), []byte(`{"model":"stub"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(agent.Result{Backend: "claude", Status: agent.StatusOK, ContextTokens: tokens}); err != nil {
				t.Fatal(err)
			}
			res := s.call(t, "run", `{"prompt":"again","backend":"codex","continue_run_id":"`+w.ID()+`"}`)
			if res.IsError {
				t.Fatal(res.Text())
			}
			recs, err := runlog.List(0)
			if err != nil {
				t.Fatal(err)
			}
			found := 0
			for _, rec := range recs {
				if rec.Backend != "claude" || rec.ID == w.ID() {
					continue
				}
				found++
				prev, err := runlog.Load(rec.Continues)
				if err != nil || prev.Backend != "codex" || prev.Result.Status != agent.StatusLimited || prev.Continues != w.ID() {
					t.Fatalf("carry-on predecessor = %+v, err=%v", prev, err)
				}
				events, err := runlog.Events(rec.ID)
				if err != nil {
					t.Fatal(err)
				}
				count := 0
				for _, e := range events {
					if e.Kind == agent.EventInfo && strings.Contains(e.Text, "short hand-over") {
						count++
						if !strings.Contains(e.Text, w.ID()) || !strings.Contains(e.Text, fmt.Sprintf("%d tokens", tokens)) {
							t.Errorf("wrong warning: %q", e.Text)
						}
					}
				}
				want := 0
				if tokens > 150000 {
					want = 1
				}
				if count != want {
					t.Errorf("carry-on recorded warnings = %d, want %d", count, want)
				}
			}
			if found != 1 {
				t.Fatalf("carry-on records = %d, want 1", found)
			}
		})
	}
}

func TestRunToolDropsRoutedByWhenALimitMovesTheRoutedRun(t *testing.T) {
	s := connect(t, t.TempDir())
	standIn(t)
	_ = limits.Note("codex", agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)})
	if err := os.WriteFile(filepath.Join(runlog.Home(), "route.json"), []byte(`{"implement":{"backend":"codex","model":"routed","effort":"medium","eval":"e1"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	res := s.call(t, "run", `{"prompt":"Implement a widget","mode":"auto"}`)
	if res.IsError {
		t.Fatalf("run: %s", res.Text())
	}
	id, _ := res.StructuredContent.(map[string]any)["run_id"].(string)
	rec, _ := runlog.Load(id)
	if rec.Backend != "claude" || rec.Task.Model != "" || rec.Task.Effort != "" || rec.RoutedBy != nil {
		t.Errorf("ran on %s with model %q effort %q routed by %+v, want claude and no routed choice", rec.Backend, rec.Task.Model, rec.Task.Effort, rec.RoutedBy)
	}
}

func TestAContinuationMovedByALimitRecordsWhy(t *testing.T) {
	s := connect(t, t.TempDir())
	standIn(t)
	first, err := runlog.Create("codex", agent.Task{Prompt: "first"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(agent.Result{Backend: "codex", Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	_ = limits.Note("codex", agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)})
	res := s.call(t, "run", `{"prompt":"go on","backend":"codex","continue_run_id":"`+first.ID()+`"}`)
	if res.IsError {
		t.Fatalf("run: %s", res.Text())
	}
	id, _ := res.StructuredContent.(map[string]any)["run_id"].(string)
	rec, _ := runlog.Load(id)
	if rec.Backend != "claude" || !strings.HasPrefix(rec.RouteNote, "continues run "+first.ID()+"; ") {
		t.Fatalf("ran on %s with route note %q: a continuation moved by a limit must say so", rec.Backend, rec.RouteNote)
	}
}

func TestRunToolCarriesOnOnTheLocalAPIWhenEveryPlanIsLimited(t *testing.T) {
	s := connect(t, t.TempDir())
	standIn(t)
	agent.Register(scripted{"claude", func(agent.Task) agent.Result {
		return agent.Result{Status: agent.StatusLimited, Error: "You've hit your usage limit.", LimitResets: time.Now().Add(time.Hour)}
	}})
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"role\":\"assistant\",\"model\":\"local\",\"usage\":{\"input_tokens\":5}}}\n\n"+
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"local answer\"}}\n\n"+
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"+
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n"+
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: []string{"codex", "claude", "api"}, LocalAPI: srv.URL}); err != nil {
		t.Fatal(err)
	}
	_ = followup.Note("claude", agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)})
	// The first call carries a limited codex run on; the second starts on api, codex's limit remembered.
	for i, want := range []string{"codex", ""} {
		res := s.call(t, "run", `{"prompt":"fix it","backend":"codex","mode":"auto"}`)
		if res.IsError {
			t.Fatalf("call %d: %s", i, res.Text())
		}
		rec, err := runlog.Load(res.StructuredContent.(map[string]any)["run_id"].(string))
		if err != nil || rec.Backend != "api" || rec.Result.Final != "local answer" {
			t.Fatalf("call %d: %+v %v", i, rec.Result, err)
		}
		limited, _ := res.StructuredContent.(map[string]any)["limited_run_id"].(string)
		if (want == "") != (limited == "") {
			t.Errorf("call %d: limited_run_id %q", i, limited)
		}
	}
	if n := hits.Load(); n != 2 {
		t.Fatalf("local requests %d, want 2", n)
	}
}

func TestBackendsToolShowsAUsableLocalAPI(t *testing.T) {
	s := connect(t, t.TempDir())
	standIn(t)
	if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: []string{"codex", "api"}, LocalAPI: "http://127.0.0.1:8081"}); err != nil {
		t.Fatal(err)
	}
	res := s.call(t, "backends", `{}`)
	var api map[string]any
	for _, e := range res.StructuredContent.(map[string]any)["backends"].([]any) {
		if m := e.(map[string]any); m["name"] == "api" {
			api = m
		}
	}
	if api["available"] != true || !strings.Contains(fmt.Sprint(api["detail"]), "local endpoint http://127.0.0.1:8081") {
		t.Fatalf("api entry %v", api)
	}
}

func TestRunToolRefusesToContinueALocalRunOnAKey(t *testing.T) {
	s := connect(t, t.TempDir())
	standIn(t)
	agent.Register(scripted{"claude", func(agent.Task) agent.Result {
		return agent.Result{Status: agent.StatusLimited, Error: "You've hit your usage limit.", LimitResets: time.Now().Add(time.Hour)}
	}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"role\":\"assistant\",\"model\":\"local\",\"usage\":{\"input_tokens\":5}}}\n\n"+
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"local answer\"}}\n\n"+
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"+
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n"+
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: []string{"codex", "claude", "api"}, LocalAPI: srv.URL}); err != nil {
		t.Fatal(err)
	}
	_ = followup.Note("claude", agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)})
	res := s.call(t, "run", `{"prompt":"fix it","backend":"codex","mode":"auto"}`)
	id, _ := res.StructuredContent.(map[string]any)["run_id"].(string)
	if rec, err := runlog.Load(id); err != nil || rec.Backend != "api" {
		t.Fatalf("carry-on %s: %v", res.Text(), err)
	}
	for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "sk-test-value")
			res, err := s.cl.CallTool(context.Background(), "run", json.RawMessage(`{"prompt":"more","continue_run_id":"`+id+`"}`))
			text := fmt.Sprint(err) + res.Text()
			if (err == nil && !res.IsError) || !strings.Contains(text, key) || strings.Contains(text, "sk-test-value") {
				t.Fatalf("continuation on a key: %s", text)
			}
		})
	}
}
