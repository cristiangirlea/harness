package execproto

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestFreshStartsANewSessionWithAHandover(t *testing.T) {
	for _, backend := range []string{"claude", "codex", "agy"} {
		t.Run(backend, func(t *testing.T) {
			registerFake(t, nil)
			got := make(chan agent.Task, 2)
			old, _ := agent.Get(backend)
			agent.Register(forkingFake{fakeBackend{name: backend, caps: agent.Capabilities{Modes: []agent.Mode{agent.ModeAuto}, Sessions: "fork", Options: []string{"model", "mode", "timeout"}},
				run: func(ctx context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
					got <- task
					return agent.Result{Backend: backend, Status: agent.StatusOK}
				}}})
			defer agent.Register(old)
			rawDir := t.TempDir()
			dir, err := filepath.EvalSymlinks(rawDir)
			if err != nil {
				t.Fatal(err)
			}
			prev, err := runlog.Create(backend, agent.Task{Prompt: "implement the parser", Dir: dir, Options: agent.Options{Model: "m1", Lean: agent.Bool(false)}})
			if err != nil {
				t.Fatal(err)
			}
			if err := prev.Close(agent.Result{Backend: backend, Status: agent.StatusOK, Session: "session-01", Final: "Parser done."}); err != nil {
				t.Fatal(err)
			}
			for _, choice := range []string{"true", "false", "absent"} {
				fresh := choice == "true" || (choice == "absent" && backend == "codex")
				h := openHarness(t, nil)
				h.next(t, "hello")
				req := start(rawDir, backend)
				req["task"].(map[string]any)["continue_run_id"] = prev.ID()
				if choice != "absent" {
					req["task"].(map[string]any)["fresh"] = choice == "true"
				}
				h.send(t, req)
				msg := h.next(t, "accepted")
				if msg["continues"] != prev.ID() {
					t.Fatalf("bad continuation: %+v", msg)
				}
				h.next(t, "event")
				if !fresh && (backend == "claude" || backend == "codex") {
					notice := h.next(t, "event")
					if text, _ := notice["event"].(map[string]any)["text"].(string); !strings.Contains(text, "--lean=false") {
						t.Fatalf("want the lean notice on a forked continuation, got %+v", notice)
					}
				}
				h.next(t, "result")
				h.exit(t, 0)
				task := <-got
				rec, err := runlog.Load(msg["run_id"].(string))
				if err != nil || rec.Continues != prev.ID() || rec.Task.Prompt != "work" {
					t.Fatalf("record: %+v %v", rec, err)
				}
				if !fresh {
					// Ordinary continue is unchanged: it forks the session; agent.Run only puts the commit block first.
					if task.ForkSession != "session-01" || rec.ContinueRoute != "continued_session" || !strings.HasPrefix(task.Prompt, "# How to commit\n\n") || !strings.HasSuffix(task.Prompt, "\nThis is a fix round: one review, one commit; every fix from this review goes into one commit.\n\nwork") {
						t.Errorf("ordinary continue changed: fork=%q route=%q prompt=%q", task.ForkSession, rec.ContinueRoute, task.Prompt)
					}
					continue
				}
				if task.ForkSession != "" || task.Resume != nil {
					t.Errorf("a fresh run picked up a session: fork=%q", task.ForkSession)
				}
				if task.Model != "m1" || !strings.Contains(task.Prompt, "> implement the parser") || !strings.HasSuffix(task.Prompt, "=== now\n\nwork") {
					t.Errorf("model %q, prompt:\n%s", task.Model, task.Prompt)
				}
				if rec.ContinueRoute != runlog.RouteFreshHandover {
					t.Errorf("route = %q", rec.ContinueRoute)
				}
				if _, err := os.Stat(filepath.Join(runlog.Root(), rec.ID, "handoff.md")); err != nil {
					t.Errorf("handoff.md: %v", err)
				}
			}
		})
	}
}

func TestFreshNeedsARunToContinue(t *testing.T) {
	registerFake(t, nil)
	h := openHarness(t, nil)
	h.next(t, "hello")
	req := start(t.TempDir(), "fake-exec")
	req["task"].(map[string]any)["fresh"] = true
	h.send(t, req)
	msg := h.next(t, "rejected")
	if e := msg["error"].(map[string]any); e["code"] != "bad_request" || !strings.Contains(e["message"].(string), "fresh needs continue_run_id") {
		t.Fatalf("fresh alone should be refused: %+v", msg)
	}
	h.exit(t, 2)
	if recs, err := runlog.List(0); err != nil || len(recs) != 0 {
		t.Fatalf("a refused start left records: %v %v", recs, err)
	}
}
