package hrnmcp

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

// sessionStub stands in for a CLI that can fork sessions and answers with
// what it was asked to pick up and the prompt it was given.
type sessionStub struct{ name string }

func (s sessionStub) Name() string            { return s.name }
func (s sessionStub) Detect() (string, error) { return "stub", nil }
func (s sessionStub) ForksSessions() bool     { return true }
func (s sessionStub) Run(_ context.Context, task agent.Task, _ func(agent.Event)) agent.Result {
	final := fmt.Sprintf("fork=%q resume=%v\n%s", task.ForkSession, task.Resume != nil, task.Prompt)
	return agent.Result{Backend: s.name, Status: agent.StatusOK, Final: final, Session: "new-session"}
}

func TestRunFreshStartsANewSessionWithAHandover(t *testing.T) {
	for _, backend := range []string{"claude", "codex", "agy"} {
		t.Run(backend, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			s := connect(t, dir)
			old, _ := agent.Get(backend)
			agent.Register(sessionStub{backend})
			defer agent.Register(old)
			w, err := runlog.Create(backend, agent.Task{Prompt: "implement the parser", Dir: dir, Options: agent.Options{Mode: agent.ModeAuto, Model: "m1"}})
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Close(agent.Result{Backend: backend, Status: agent.StatusOK, Session: "s1", Final: "Parser done."}); err != nil {
				t.Fatal(err)
			}
			r := s.call(t, "run", `{"prompt":"review it","no_switch":true,"fresh":true,"continue_run_id":"`+w.ID()+`"}`)
			if r.IsError {
				t.Fatal(r.Text())
			}
			for _, want := range []string{`fork="" resume=false`, "> implement the parser", "> Parser done.", "=== now\n\nreview it"} {
				if !strings.Contains(r.Text(), want) {
					t.Errorf("the backend was not given %q:\n%s", want, r.Text())
				}
			}
			next := r.StructuredContent.(map[string]any)["run_id"].(string)
			rec, err := runlog.Load(next)
			if err != nil || rec.Continues != w.ID() || rec.ContinueRoute != runlog.RouteFreshHandover || rec.Task.Prompt != "review it" {
				t.Fatalf("record should link the run and say the route: %+v err=%v", rec, err)
			}
			if rec.Task.Mode != agent.ModeAuto || rec.Task.Model != "m1" || rec.Backend != backend {
				t.Errorf("mode, model and backend should be inherited: %+v", rec.Task.Options)
			}
			if _, err := os.Stat(filepath.Join(runlog.Root(), next, "handoff.md")); err != nil {
				t.Errorf("handoff.md: %v", err)
			}
			if backend == "codex" {
				defaultRun := s.call(t, "run", `{"prompt":"again","no_switch":true,"continue_run_id":"`+w.ID()+`"}`)
				if defaultRun.IsError || !strings.Contains(defaultRun.Text(), `fork=""`) || !strings.Contains(defaultRun.Text(), "=== now") {
					t.Fatalf("codex default should hand over: %s", defaultRun.Text())
				}
			}
			// An explicit false keeps the session.
			r = s.call(t, "run", `{"prompt":"again","no_switch":true,"fresh":false,"continue_run_id":"`+w.ID()+`"}`)
			if r.IsError {
				t.Fatal(r.Text())
			}
			if strings.Contains(r.Text(), `fork=""`) || strings.Contains(r.Text(), "=== now") {
				t.Errorf("ordinary continue changed:\n%s", r.Text())
			}
		})
	}
}

func TestRunFreshNeedsARunToContinue(t *testing.T) {
	s := connect(t, t.TempDir())
	r := s.call(t, "run", `{"prompt":"x","backend":"stub","fresh":true}`)
	if !r.IsError || !strings.Contains(r.Text(), "fresh needs continue_run_id") {
		t.Fatalf("fresh alone should be refused: %s", r.Text())
	}
	if recs, err := runlog.List(0); err != nil || len(recs) != 0 {
		t.Fatalf("a refused run left records: %v %v", recs, err)
	}
}

// resumeStub resumes a conversation in place, as agy does, and tries to plan a
// second continuation of prev while it runs.
type resumeStub struct {
	sessionStub
	prev    string
	refused *error
}

func (s resumeStub) Capabilities() agent.Capabilities {
	return agent.Capabilities{Modes: []agent.Mode{agent.ModeReadOnly, agent.ModeAuto, agent.ModeYolo}, Sessions: "resume"}
}

func (s resumeStub) Run(ctx context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
	p, err := followup.Make(followup.Request{RunID: s.prev})
	if err == nil {
		p.Release()
	}
	*s.refused = err
	res := s.sessionStub.Run(ctx, task, emit)
	res.Session = task.ForkSession
	return res
}

func TestRunHoldsAnAgyConversationUntilItEnds(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := connect(t, dir)
	w, err := runlog.Create("agy", agent.Task{Prompt: "implement the parser", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{Backend: "agy", Status: agent.StatusOK, Session: "conversation-1", Final: "Parser done."}); err != nil {
		t.Fatal(err)
	}
	var refused error
	old, _ := agent.Get("agy")
	agent.Register(resumeStub{sessionStub{"agy"}, w.ID(), &refused})
	defer agent.Register(old)
	for range 2 {
		refused = nil
		r := s.call(t, "run", `{"prompt":"again","no_switch":true,"continue_run_id":"`+w.ID()+`"}`)
		if r.IsError {
			t.Fatalf("the conversation was not released after the last run: %s", r.Text())
		}
		if refused == nil || !strings.Contains(refused.Error(), "another hrn run") {
			t.Fatalf("a continuation during the run: %v", refused)
		}
	}
}

func TestRunReleasesAnAgyConversationAfterCarryingALimitedRunOn(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := connect(t, dir)
	if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: []string{"agy", "codex"}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range limits.MeteredEnv("codex") {
		t.Setenv(name, "")
	}
	w, err := runlog.Create("agy", agent.Task{Prompt: "implement the parser", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{Backend: "agy", Status: agent.StatusOK, Session: "conversation-1", Final: "Parser done."}); err != nil {
		t.Fatal(err)
	}
	var refused error
	for _, b := range []agent.Backend{
		resumeStub{sessionStub{"agy"}, w.ID(), &refused},
		scripted{"codex", func(agent.Task) agent.Result {
			return agent.Result{Status: agent.StatusLimited, Error: "You've hit your usage limit.", LimitResets: time.Now().Add(time.Hour)}
		}},
	} {
		old, _ := agent.Get(b.Name())
		agent.Register(b)
		defer agent.Register(old)
	}
	r := s.call(t, "run", `{"prompt":"again","backend":"codex","continue_run_id":"`+w.ID()+`"}`)
	if r.IsError || !strings.Contains(r.Text(), "codex reached its usage limit") {
		t.Fatalf("agy did not carry the limited run on: %s", r.Text())
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
