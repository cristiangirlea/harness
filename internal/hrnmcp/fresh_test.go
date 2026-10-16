package hrnmcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
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
			// Without fresh the same call forks the session, as before.
			r = s.call(t, "run", `{"prompt":"again","no_switch":true,"continue_run_id":"`+w.ID()+`"}`)
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
