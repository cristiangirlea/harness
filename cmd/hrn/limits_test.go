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
