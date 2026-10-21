package eval

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
)

type launchStub struct {
	stub
	failStarts int
	failTask   bool
	tasks      []agent.Task
}

func (s *launchStub) Run(ctx context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
	s.tasks = append(s.tasks, task)
	if len(s.tasks) <= s.failStarts {
		return agent.Result{Backend: s.name, Status: agent.StatusError, Error: "launch refused", CostUSD: .125}
	}
	r := s.stub.Run(ctx, task, emit)
	if s.failTask {
		r.Status, r.Error = agent.StatusError, "task failed"
	}
	return r
}

func TestRunRerunsOnlyUnstartedCells(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, tc := range []struct {
		name       string
		policy     string
		failStarts int
		failTask   bool
		calls      int
		passed     int
		unstarted  int
		reruns     int
		exhausted  int
	}{
		{"recovered", "", 1, false, 2, 1, 0, 1, 0},
		{"exhausted", `,"rerun_unstarted":2`, 9, false, 3, 0, 1, 1, 1},
		{"disabled", `,"rerun_unstarted":0`, 9, false, 1, 0, 1, 0, 0},
		{"ran-failed", "", 0, true, 1, 0, 0, 0, 0},
		{"recovered-failed", "", 1, true, 2, 0, 0, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			dir := initRepo(t)
			b := &launchStub{stub: stub{"ev-launch-" + tc.name, true}, failStarts: tc.failStarts, failTask: tc.failTask}
			agent.Register(b)
			var spec Spec
			if err := json.Unmarshal([]byte(`{"parallel":1,"matrix":[{"backend":"`+b.name+`","model":"chosen","effort":"high","prompt_prefix":"prefix"}],"tasks":[{"id":"task","prompt":"original","expect":"done"}]`+tc.policy+`}`), &spec); err != nil {
				t.Fatal(err)
			}
			spec.Dir = dir
			rec, res, err := Run(context.Background(), spec, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := batch.Remove(res); err != nil {
					t.Error(err)
				}
			})
			if len(b.tasks) != tc.calls || len(res.Tasks) != tc.calls || len(rec.Results) != tc.calls {
				t.Fatalf("calls=%d tasks=%d recorded=%d, want %d", len(b.tasks), len(res.Tasks), len(rec.Results), tc.calls)
			}
			v, row := rec.Table.Variants[0], rec.Table.Tasks[0]
			if v.Attempts != 1 || v.Passed != tc.passed || v.DidNotStart != tc.unstarted || v.Reruns != tc.reruns || v.RerunsExhausted != tc.exhausted {
				t.Errorf("variant = %+v", v)
			}
			if row.Total != 1 || row.Passed != tc.passed || row.DidNotStart != tc.unstarted || row.Reruns != tc.reruns || row.RerunsExhausted != tc.exhausted {
				t.Errorf("task row = %+v", row)
			}
			wantRate := 1.0
			if tc.failStarts > 0 {
				wantRate = 0
			}
			if v.BackendStartRate != wantRate || v.PassRate() != float64(tc.passed) {
				t.Errorf("start rate=%v pass rate=%v", v.BackendStartRate, v.PassRate())
			}
			wantCost := float64(min(tc.calls, tc.failStarts)) * .125
			if tc.calls > tc.failStarts {
				wantCost += .25
			}
			if cells, passed, cost, _, _, _ := rec.Totals(); cells != 1 || passed != tc.passed || cost != wantCost {
				t.Errorf("totals = %d cells, %d passed, %v cost; want cost %v", cells, passed, cost, wantCost)
			}
			for i, task := range b.tasks {
				if task.OriginalPrompt != "prefix\n\noriginal" || task.Prompt != b.tasks[0].Prompt || !reflect.DeepEqual(task.Options, b.tasks[0].Options) || task.Model != "chosen" || task.Effort != "high" {
					t.Errorf("call %d changed prompt or options", i)
				}
				if i > 0 && res.Tasks[i].ID != res.Tasks[0].ID+".u"+strconv.Itoa(i) {
					t.Errorf("rerun id = %q", res.Tasks[i].ID)
				}
			}
			loaded, err := LoadRecord(rec.ID)
			if err != nil || len(loaded.Results) != tc.calls {
				t.Fatalf("load: %v, results=%d", err, len(loaded.Results))
			}
			if tc.failStarts > 0 && loaded.Results[0].Attempts[0].DidNotStart != "launch refused" {
				t.Error("original diagnostics lost")
			}
			if !reflect.DeepEqual(loaded.Results, rec.Results) {
				t.Error("raw results changed on disk")
			}
		})
	}
}

func TestRerunPolicyRoundTripsAndRejectsNegative(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, value := range []int{0, 1, 2} {
		spec := testSpec("unused")
		spec.RerunUnstarted = value
		data, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		var loaded Spec
		if err := json.Unmarshal(data, &loaded); err != nil || loaded.RerunUnstarted != value {
			t.Errorf("policy %d: JSON %s, loaded %d, error %v", value, data, loaded.RerunUnstarted, err)
		}
	}
	spec := testSpec("unused")
	spec.RerunUnstarted = -1
	if _, err := Expand(spec, "e"); err == nil || !strings.Contains(err.Error(), "rerun_unstarted") {
		t.Errorf("negative policy: %v", err)
	}
}

func TestRerunsWaitForAllScheduledCells(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := initRepo(t)
	b := &launchStub{stub: stub{"ev-launch-rounds", false}, failStarts: 1}
	agent.Register(b)
	spec := Spec{Spec: batch.Spec{Dir: dir, Parallel: 1, Tasks: []batch.Task{
		{ID: "one", Prompt: "first", Expect: "done"},
		{ID: "two", Prompt: "second", Expect: "done"},
		{ID: "three", Prompt: "third", Expect: "done"},
	}}, Matrix: []Variant{{Backend: b.name}}, RerunUnstarted: 2}
	rec, res, err := Run(context.Background(), spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := batch.Remove(res); err != nil {
			t.Error(err)
		}
	})
	if len(b.tasks) != 4 {
		t.Fatalf("calls = %d, want 4", len(b.tasks))
	}
	for i, want := range []string{"first", "second", "third", "first"} {
		if b.tasks[i].OriginalPrompt != want {
			t.Errorf("call %d original prompt = %q, want %q", i, b.tasks[i].OriginalPrompt, want)
		}
	}
	v := rec.Table.Variants[0]
	if v.Attempts != 3 || v.Passed != 3 || v.Reruns != 1 || v.RerunsExhausted != 0 || v.BackendStartRate != 2.0/3 || v.PassRate() != 1 {
		t.Errorf("variant = %+v", v)
	}
}
