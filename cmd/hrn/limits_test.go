package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/followup"
	"github.com/cristiangirlea/harness/internal/limits"
	"github.com/cristiangirlea/harness/internal/prefs"
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
// so no real CLI can run, and restores them afterwards. codex reaches its
// limit; claude finishes.
func standIn(t *testing.T) {
	t.Helper()
	// A key in the environment would keep the partner from standing in.
	for _, name := range append(limits.MeteredEnv("claude"), limits.MeteredEnv("codex")...) {
		t.Setenv(name, "")
	}
	for _, s := range []scripted{
		{"codex", func(agent.Task) agent.Result {
			return agent.Result{Status: agent.StatusLimited, Error: "You've hit your usage limit.",
				Final: "Half done.", LimitResets: time.Now().Add(time.Hour)}
		}},
		{"claude", func(agent.Task) agent.Result {
			return agent.Result{Status: agent.StatusOK, Final: "finished", Turns: 1}
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

func TestRunCarriesALimitedRunOnOnTheOtherBackend(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t)
	code, err := cmdRun([]string{"-b", "codex", "-C", t.TempDir(), "--mode", "auto", "fix", "it"})
	if code != 0 || err != nil {
		t.Fatalf("code=%d err=%v: the carried-on run finished", code, err)
	}
	recs, err := runlog.List(5)
	if err != nil || len(recs) != 2 {
		t.Fatalf("runs = %d (%v), want the limited one and its carry-on", len(recs), err)
	}
	// Ids made in the same second sort by their random part, so by backend.
	limited, carried := recs[1], recs[0]
	if carried.Backend == "codex" {
		limited, carried = carried, limited
	}
	if limited.Backend != "codex" || limited.Result.Status != agent.StatusLimited {
		t.Errorf("first run: %s %s", limited.Backend, limited.Result.Status)
	}
	if carried.Backend != "claude" || carried.Continues != limited.ID || carried.Result.Status != agent.StatusOK {
		t.Errorf("second run: %s continues %q, %s", carried.Backend, carried.Continues, carried.Result.Status)
	}
	if carried.Task.Mode != agent.ModeAuto {
		t.Errorf("mode = %s, want the limited run's", carried.Task.Mode)
	}
	if limits.Until("codex", time.Now()).IsZero() {
		t.Error("codex's limit was not remembered")
	}
}

func TestCarryOnTurnCapFollowsBackend(t *testing.T) {
	for _, from := range []string{"claude", "codex"} {
		t.Run(from, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			standIn(t)
			to, want := "claude", 30
			if from == "claude" {
				to, want = "codex", 0
			}
			got := -99
			agent.Register(scripted{from, func(agent.Task) agent.Result {
				return agent.Result{Status: agent.StatusLimited, Error: "You've hit your usage limit.", LimitResets: time.Now().Add(time.Hour)}
			}})
			agent.Register(scripted{to, func(task agent.Task) agent.Result { got = task.MaxTurns; return agent.Result{Status: agent.StatusOK} }})
			code, err := cmdRun([]string{"-b", from, "-C", t.TempDir(), "--mode", "auto", "--purpose", "review", "work"})
			if code != 0 || err != nil || got != want {
				t.Fatalf("code=%d err=%v cap=%d want=%d", code, err, got, want)
			}
			recs, err := runlog.List(5)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, rec := range recs {
				if rec.Backend == to {
					found = true
					if rec.Task.MaxTurns != want {
						t.Errorf("record cap=%d want=%d", rec.Task.MaxTurns, want)
					}
				}
			}
			if !found {
				t.Fatal("missing carry-on record")
			}
		})
	}
}

func TestRunStartsOnTheOtherBackendWhileALimitIsRemembered(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t)
	_ = limits.Note("codex", agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)})
	code, err := cmdRun([]string{"-b", "codex", "-m", "gpt-5.5", "-C", t.TempDir(), "fix", "it"})
	if code != 0 || err != nil {
		t.Fatalf("code=%d err=%v", code, err)
	}
	recs, _ := runlog.List(5)
	if len(recs) != 1 || recs[0].Backend != "claude" {
		t.Fatalf("runs = %+v, want one on claude", recs)
	}
	if recs[0].Task.Model != "" {
		t.Errorf("model = %q: the one typed was for codex", recs[0].Task.Model)
	}
	events, _ := runlog.Events(recs[0].ID)
	said := false
	for _, e := range events {
		said = said || e.Kind == agent.EventInfo && strings.Contains(e.Text, "codex is at its usage limit")
	}
	if !said {
		t.Error("the run does not say why it started on claude")
	}
}

func TestRunExplainsAMeteredPartnerBeforeStartingAndKeepsItsModel(t *testing.T) {
	for _, continuing := range []bool{false, true} {
		t.Run(fmt.Sprint(continuing), func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			standIn(t)
			t.Setenv("ANTHROPIC_API_KEY", "fake-secret-value")
			dir := t.TempDir()
			args := []string{"-b", "codex", "-m", "requested-model", "-C", dir}
			if continuing {
				rec, err := runlog.Create("codex", agent.Task{Prompt: "first", Dir: dir})
				if err != nil {
					t.Fatal(err)
				}
				if err := rec.Close(agent.Result{Backend: "codex", Status: agent.StatusOK}); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(rec.Dir(), "messages.json"), []byte(`{"model":"previous-model"}`), 0o644); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--continue", rec.ID())
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
			if code, err := cmdRun(append(args, "fix it")); code != 0 || err != nil || !called {
				t.Fatalf("code=%d err=%v called=%v", code, err, called)
			}
		})
	}
}

func TestRunDoesNotCreateACarryOnAfterCancellation(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agent.Register(scripted{"codex", func(agent.Task) agent.Result {
		cancel()
		return agent.Result{Status: agent.StatusLimited, Error: "usage limit", LimitResets: time.Now().Add(time.Hour)}
	}})
	code, err := cmdRunContext(ctx, []string{"-b", "codex", "-C", t.TempDir(), "fix it"})
	if code != 1 || err != nil {
		t.Errorf("code=%d err=%v, want the limited run's exit code", code, err)
	}
	recs, err := runlog.List(5)
	if err != nil || len(recs) != 1 || recs[0].Backend != "codex" || recs[0].Result.Status != agent.StatusLimited {
		t.Fatalf("records = %+v, err=%v; want only the limited run", recs, err)
	}
	if limits.Until("codex", time.Now()).IsZero() {
		t.Error("codex's limit was not remembered in limits.json")
	}
}

func TestNoSwitchKeepsTheRunOnItsBackend(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t)
	// Remembered or not, the run stays.
	_ = limits.Note("codex", agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)})
	code, _ := cmdRun([]string{"-b", "codex", "--no-switch", "-C", t.TempDir(), "fix", "it"})
	if code != 1 {
		t.Errorf("code = %d, want 1 for a limited run", code)
	}
	if recs, _ := runlog.List(5); len(recs) != 1 || recs[0].Backend != "codex" {
		t.Errorf("runs = %+v, want only the codex one", recs)
	}
}

func TestContinuingALimitedBackendGoesToTheOtherOne(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t)
	dir := t.TempDir()
	// A finished claude run, then claude's limit remembered.
	if code, err := cmdRun([]string{"-b", "claude", "-C", dir, "first"}); code != 0 || err != nil {
		t.Fatalf("code=%d err=%v", code, err)
	}
	recs, _ := runlog.List(1)
	first := recs[0].ID
	_ = limits.Note("claude", agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)})

	// The codex stand-in ends limited, and claude is limited too, so nothing
	// carries it further; what matters is that the follow-up went to codex.
	cmdRun([]string{"--continue", first, "again"})
	recs, _ = runlog.List(5)
	if len(recs) != 2 {
		t.Fatalf("runs = %d, want the first and one follow-up", len(recs))
	}
	follow := recs[0]
	if follow.ID == first {
		follow = recs[1]
	}
	if follow.Backend != "codex" || follow.Continues != first {
		t.Errorf("follow-up on %s continuing %q, want codex continuing %s", follow.Backend, follow.Continues, first)
	}
}

func TestWhenBothReachTheirLimitBothAreRemembered(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t)
	orig, _ := agent.Get("claude")
	agent.Register(scripted{"claude", func(agent.Task) agent.Result {
		return agent.Result{Status: agent.StatusLimited, Error: "You've hit your session limit.", LimitResets: time.Now().Add(2 * time.Hour)}
	}})
	t.Cleanup(func() { agent.Register(orig) })

	if code, _ := cmdRun([]string{"-b", "codex", "-C", t.TempDir(), "fix", "it"}); code != 1 {
		t.Errorf("code = %d, want 1: both ended limited", code)
	}
	if recs, _ := runlog.List(5); len(recs) != 2 {
		t.Errorf("runs = %d, want the limited run and one carry-on, no third", len(recs))
	}
	for _, name := range []string{"codex", "claude"} {
		if limits.Until(name, time.Now()).IsZero() {
			t.Errorf("%s's limit was not remembered", name)
		}
	}
}

func TestLimitCarryOnRecordsContextWarning(t *testing.T) {
	for _, tokens := range []int64{150000, 150001} {
		t.Run(fmt.Sprint(tokens), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HRN_HOME", t.TempDir())
			standIn(t)
			w, err := runlog.Create("claude", agent.Task{Prompt: "first", Dir: dir})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(w.Dir(), "messages.json"), []byte(`{"model":"stub"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(agent.Result{Backend: "claude", Status: agent.StatusOK, ContextTokens: tokens}); err != nil {
				t.Fatal(err)
			}
			code, err := cmdRun([]string{"--continue", w.ID(), "-b", "codex", "again"})
			if code != 0 || err != nil {
				t.Fatalf("carry-on: code=%d err=%v", code, err)
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

// resumingAgy resumes its conversation in place and tries a second
// continuation of prev while it runs.
type resumingAgy struct {
	scripted
	prev    string
	refused *error
}

func (s resumingAgy) ForksSessions() bool { return true }
func (s resumingAgy) Capabilities() agent.Capabilities {
	return agent.Capabilities{Modes: []agent.Mode{agent.ModeReadOnly, agent.ModeAuto, agent.ModeYolo}, Sessions: "resume"}
}
func (s resumingAgy) Run(ctx context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
	p, err := followup.Make(followup.Request{RunID: s.prev})
	if err == nil {
		p.Release()
	}
	*s.refused = err
	res := s.scripted.Run(ctx, task, emit)
	res.Session = task.ForkSession
	return res
}

func TestRunReleasesAnAgyConversationAfterCarryingALimitedRunOn(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t)
	if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: []string{"agy", "codex"}}); err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := runlog.Create("agy", agent.Task{Prompt: "implement the parser", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{Backend: "agy", Status: agent.StatusOK, Session: "conversation-1", Final: "Parser done."}); err != nil {
		t.Fatal(err)
	}
	var refused error
	orig, err := agent.Get("agy")
	if err != nil {
		t.Fatal(err)
	}
	agent.Register(resumingAgy{scripted{"agy", func(agent.Task) agent.Result {
		return agent.Result{Status: agent.StatusOK, Final: "finished", Turns: 1}
	}}, w.ID(), &refused})
	t.Cleanup(func() { agent.Register(orig) })

	code, err := cmdRun([]string{"-b", "codex", "--continue", w.ID(), "again"})
	if code != 0 || err != nil {
		t.Fatalf("code=%d err=%v: agy should have carried the limited run on", code, err)
	}
	if refused == nil || !strings.Contains(refused.Error(), "another hrn run") {
		t.Fatalf("the carry-on did not hold the conversation: %v", refused)
	}
	p, err := followup.Make(followup.Request{RunID: w.ID()})
	if err != nil {
		t.Fatalf("the conversation was not released after the carry-on: %v", err)
	}
	p.Release()
}
