package execproto

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/flow"
	"github.com/cristiangirlea/harness/internal/route/table"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestStartRecordsBackendRouting(t *testing.T) {
	for _, tc := range []struct {
		name, prompt, purpose string
		row, named            bool
	}{
		{name: "purpose row", prompt: "work", purpose: "review", row: true},
		{name: "prompt header row", prompt: "Purpose: review\nwork", row: true},
		{name: "default", prompt: "work", purpose: "review"},
		{name: "named backend", prompt: "work", purpose: "review", row: true, named: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			var got agent.Task
			name := registerFake(t, func(_ context.Context, task agent.Task, _ func(agent.Event)) agent.Result {
				got = task
				return agent.Result{Backend: "fake-exec", Status: agent.StatusOK}
			})
			backend := name
			if !tc.named {
				backend = "codex"
				if !tc.row {
					backend = agent.DefaultBackendNameIn(runlog.Home())
				}
				original, err := agent.Get(backend)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { agent.Register(original) })
				fake, _ := agent.Get(name)
				b := fake.(fakeBackend)
				b.name = backend
				b.run = func(_ context.Context, task agent.Task, _ func(agent.Event)) agent.Result {
					got = task
					return agent.Result{Backend: backend, Status: agent.StatusOK}
				}
				agent.Register(b)
			}
			if tc.row {
				if err := table.Save(runlog.Home(), table.Table{"review": {Backend: "codex", Model: "routed-model", Effort: "high", Eval: "eval-test", Set: "manual"}}); err != nil {
					t.Fatal(err)
				}
			}
			h := openHarness(t, nil)
			h.next(t, "hello")
			req := start(t.TempDir(), name)
			task := req["task"].(map[string]any)
			task["prompt"] = tc.prompt
			if tc.purpose != "" {
				task["purpose"] = tc.purpose
			}
			if !tc.named {
				delete(task, "backend")
			}
			h.send(t, req)
			accepted := h.next(t, "accepted")
			requested := accepted["requested"].(map[string]any)
			if requested["backend"] != backend {
				t.Fatalf("accepted backend = %v, want %s", requested["backend"], backend)
			}
			h.next(t, "result")
			h.exit(t, 0)
			rec, err := runlog.Load(accepted["run_id"].(string))
			if err != nil {
				t.Fatal(err)
			}
			wantNote := ""
			var wantBy *runlog.RoutedBy
			if tc.named {
				wantNote = "backend " + backend + " was named for this run"
			} else if tc.row {
				wantBy = &runlog.RoutedBy{Category: "review", Declared: true, Eval: "eval-test", Set: "manual"}
			} else {
				wantNote = "no route row for purpose review; default backend " + backend + " was used"
			}
			if rec.Backend != backend || !reflect.DeepEqual(rec.RoutedBy, wantBy) || rec.RouteNote != wantNote {
				t.Fatalf("routing = %s %+v %q, want %s %+v %q", rec.Backend, rec.RoutedBy, rec.RouteNote, backend, wantBy, wantNote)
			}
			model, effort := "", ""
			if wantBy != nil {
				model, effort = "routed-model", "high"
			}
			if got.Model != model || got.Effort != effort || requested["model"] != model || requested["effort"] != effort {
				t.Fatalf("resolved options: task %+v, accepted %+v", got.Options, requested)
			}
			events, err := runlog.Events(rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			steps := flow.FromRun(rec, events, "")
			if len(steps) != 3 || steps[1].Kind != flow.KindRoute || steps[1].Name != backend || steps[1].Reason == "" || steps[1].Reason != steps[0].Reason || steps[1].Owner != rec.Owner {
				t.Fatalf("flow routing: %+v", steps)
			}
		})
	}
}

func TestOldRecordWithoutRoutingFieldsLoadsIntoFlow(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	id := "20260101-000000-aaaaaa"
	dir := filepath.Join(runlog.Root(), id)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf(`{"id":%q,"backend":"old","task":{"prompt":"old","dir":"."},"result":{"backend":"old","status":"ok"}}`, id)
	if err := os.WriteFile(filepath.Join(dir, "result.json"), []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
	rec, err := runlog.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Done || rec.RoutedBy != nil || rec.RouteNote != "" {
		t.Fatalf("old record: %+v", rec)
	}
	steps := flow.FromRun(rec, nil, "")
	if len(steps) != 2 || steps[0].Kind != flow.KindRun || steps[0].Name != "old" || steps[0].Reason != "" || steps[1].Status != flow.StatusOK {
		t.Fatalf("old record flow: %+v", steps)
	}
}

func TestAContinuationWithoutABackendStaysOnTheEarlierRunsBackend(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	var got agent.Task
	name := registerFake(t, func(_ context.Context, task agent.Task, _ func(agent.Event)) agent.Result {
		got = task
		return agent.Result{Backend: "fake-exec", Status: agent.StatusOK}
	})
	if err := table.Save(runlog.Home(), table.Table{"review": {Backend: "codex", Model: "routed-model", Set: "manual"}}); err != nil {
		t.Fatal(err)
	}
	first, err := runlog.Create(name, agent.Task{Prompt: "first", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(agent.Result{Backend: name, Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	h := openHarness(t, nil)
	h.next(t, "hello")
	h.send(t, map[string]any{"type": "start", "attempt_id": "a1", "task": map[string]any{
		"prompt": "work", "purpose": "review", "dir": t.TempDir(), "mode": "auto", "continue_run_id": first.ID(), "fresh": true}})
	accepted := h.next(t, "accepted")
	if b, _ := accepted["requested"].(map[string]any)["backend"].(string); b != name {
		t.Fatalf("accepted backend = %q, want the earlier run's %q, not the route row's codex", b, name)
	}
	// The hand-over's info event precedes the result.
	for msg := h.next(t, "event"); msg["type"] != "result"; msg = h.nextAny(t) {
	}
	if got.Model != "" {
		t.Fatalf("a continuation must not take the route row's model, got %q", got.Model)
	}
	recs, _ := runlog.List(1)
	if len(recs) != 1 || recs[0].Backend != name || recs[0].RouteNote != "" || recs[0].RoutedBy != nil {
		t.Fatalf("record = %+v", recs[0])
	}
}
