package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// continued returns the record of the run that continues id.
func continued(t *testing.T, id string) runlog.Record {
	t.Helper()
	recs, err := runlog.List(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		if rec.Continues == id {
			return rec
		}
	}
	t.Fatalf("no run continues %s", id)
	return runlog.Record{}
}

func TestRunFreshStartsANewSessionWithAHandover(t *testing.T) {
	for _, backend := range []string{"claude", "codex", "agy"} {
		t.Run(backend, func(t *testing.T) {
			drain(t)
			t.Setenv("HRN_HOME", t.TempDir())
			old, _ := agent.Get(backend)
			agent.Register(recorder{backend, recorded})
			defer agent.Register(old)
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			w, err := runlog.Create(backend, agent.Task{Prompt: "implement the parser", Dir: dir, Options: agent.Options{Mode: agent.ModeAuto, Model: "m1"}})
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Close(agent.Result{Backend: backend, Status: agent.StatusOK, Session: "s1", Final: "Parser done."}); err != nil {
				t.Fatal(err)
			}
			code, err := cmdRun([]string{"--continue", w.ID(), "--fresh", "--no-switch", "review it"})
			if code != 0 || err != nil {
				t.Fatalf("code=%d err=%v", code, err)
			}
			task := <-recorded
			if task.ForkSession != "" || task.Resume != nil {
				t.Fatalf("a fresh run picked up a session: fork=%q resume=%v", task.ForkSession, task.Resume)
			}
			if task.Mode != agent.ModeAuto || task.Model != "m1" || task.Dir != dir {
				t.Errorf("mode, model and workspace should be inherited: %+v", task.Options)
			}
			if !strings.Contains(task.Prompt, "> implement the parser") || !strings.Contains(task.Prompt, "> Parser done.") || !strings.HasSuffix(task.Prompt, "=== now\n\nreview it") {
				t.Errorf("prompt should be the hand-over, then the request:\n%s", task.Prompt)
			}
			rec := continued(t, w.ID())
			if rec.ContinueRoute != runlog.RouteFreshHandover || rec.Task.Prompt != "review it" || rec.Backend != backend {
				t.Errorf("record: route=%q prompt=%q backend=%q", rec.ContinueRoute, rec.Task.Prompt, rec.Backend)
			}
			saved, err := os.ReadFile(filepath.Join(runlog.Root(), rec.ID, "handoff.md"))
			if err != nil || string(saved)+"review it" != task.Prompt {
				t.Errorf("handoff.md: %v", err)
			}
		})
	}
}

func TestRunWithoutFreshStillResumes(t *testing.T) {
	drain(t)
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := runlog.Create("stub-cmd", agent.Task{Prompt: "first", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir(), "messages.json"), []byte(`{"model":"stub"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{Backend: "stub-cmd", Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	if code, err := cmdRun([]string{"--continue", w.ID(), "--no-switch", "again"}); code != 0 || err != nil {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if task := <-recorded; task.Resume == nil || task.Prompt != "again" {
		t.Errorf("ordinary continue should resume the conversation as before: resume=%v prompt=%q", task.Resume, task.Prompt)
	}
	if rec := continued(t, w.ID()); rec.ContinueRoute != "" {
		t.Errorf("ordinary continue recorded route %q", rec.ContinueRoute)
	}
	if code, err := cmdRun([]string{"--continue", w.ID(), "--fresh", "--no-switch", "again"}); code != 0 || err != nil {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if task := <-recorded; task.Resume != nil {
		t.Errorf("a fresh run must not resume messages.json")
	}
}

func TestRunFreshNeedsARunToContinue(t *testing.T) {
	drain(t)
	t.Setenv("HRN_HOME", t.TempDir())
	code, err := cmdRun([]string{"-b", "stub-cmd", "-C", t.TempDir(), "--fresh", "x"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "--fresh needs --continue") {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if recs, err := runlog.List(0); err != nil || len(recs) != 0 {
		t.Fatalf("a refused run left records: %v %v", recs, err)
	}
}
