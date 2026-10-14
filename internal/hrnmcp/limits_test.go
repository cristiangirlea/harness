package hrnmcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/limits"
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
	for _, name := range append(limits.MeteredEnv("claude"), limits.MeteredEnv("codex")...) {
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
