package route

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/eval"
)

func TestRouteTableRoundTripsAndNamesTheFileWhenBroken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	tab, err := Load()
	if err != nil || len(tab) != 0 {
		t.Fatalf("missing: %v %v", tab, err)
	}
	want := Table{"implement": {Backend: "codex", Model: "sol", Effort: "medium", Eval: "e1", Set: "2026-10-14"}}
	if err := Save(want); err != nil {
		t.Fatal(err)
	}
	tab, err = Load()
	if err != nil || !reflect.DeepEqual(tab, want) {
		t.Fatalf("round trip: %v %v", tab, err)
	}
	if row, ok := tab.Pick("implement"); !ok || row != want["implement"] {
		t.Fatalf("pick: %v %v", row, ok)
	}
	if _, ok := tab.Pick("debug"); ok {
		t.Fatal("missing row picked")
	}
	if err := os.WriteFile(filepath.Join(home, "route.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), filepath.Join(home, "route.json")) {
		t.Fatalf("broken: %v", err)
	}
}

func evidence() eval.Record {
	return eval.Record{ID: "e1", Started: time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC), Spec: eval.Spec{
		Spec:   batch.Spec{Tasks: []batch.Task{{ID: "build", Prompt: "Implement a widget"}, {ID: "check", Prompt: "Review the patch"}}},
		Matrix: []eval.Variant{{Name: "high", Backend: "codex", Model: "sol", Effort: "high"}, {Name: "low", Backend: "codex", Model: "sol", Effort: "low"}, {Name: "small", Backend: "api", Model: "small", Effort: "low"}},
	}, Table: eval.Table{
		Variants: []eval.VariantRow{{Name: "high", MedianCostUSD: 3}, {Name: "low", MedianCostUSD: 2}, {Name: "small", MedianCostUSD: 1}},
		Tasks:    []eval.TaskRow{{Task: "build", Variant: "high", Passed: 10, Total: 10}, {Task: "build", Variant: "low", Passed: 10, Total: 10}, {Task: "build", Variant: "small", Passed: 9, Total: 10}, {Task: "check", Variant: "high", Passed: 10, Total: 10}, {Task: "check", Variant: "low", Passed: 8, Total: 10}, {Task: "check", Variant: "small", Passed: 7, Total: 10}},
	}}
}

func TestFromEvalPurposeMarginAndEffort(t *testing.T) {
	r := evidence()
	tab := FromEval(r, .05)
	if len(tab) != 2 || tab["implement"].Effort != "low" || tab["implement"].Model != "sol" || tab["review"].Effort != "high" {
		t.Fatalf("routes: %+v", tab)
	}
	if tab["implement"].Eval != "e1" || tab["implement"].Set != "2026-10-12" {
		t.Fatalf("provenance: %+v", tab)
	}
	if got := FromEval(r, .1)["implement"]; got.Model != "small" {
		t.Fatalf("margin: %+v", got)
	}
	if got := FromEval(eval.Record{}, .05); len(got) != 0 {
		t.Fatalf("empty: %v", got)
	}
}

func TestFromEvalPrefersPlanAtEqualCostAndRate(t *testing.T) {
	r := evidence()
	r.Spec.Matrix[2].Model = "sol"
	r.Table.Variants[2].MedianCostUSD = 2
	r.Table.Tasks[2].Passed = 10
	if got := FromEval(r, 0)["implement"]; got.Backend != "codex" {
		t.Fatalf("tie: %+v", got)
	}
}

func TestFromEvalSkipsMissingAndNonFiniteCosts(t *testing.T) {
	for _, tc := range []struct {
		name string
		cost float64
	}{
		{"missing", 0}, {"nan", math.NaN()}, {"positive-infinity", math.Inf(1)}, {"negative-infinity", math.Inf(-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := evidence()
			if tc.name == "missing" {
				r.Table.Variants = r.Table.Variants[:2]
			} else {
				r.Table.Variants[2].MedianCostUSD = tc.cost
			}
			if got := FromEval(r, .1)["implement"]; got.Backend != "codex" || got.Model != "sol" || got.Effort != "low" {
				t.Fatalf("invalid cost selected: %+v", got)
			}
		})
	}
}

func TestFromEvalPlanPreferencePrecedesEffortAcrossFamilies(t *testing.T) {
	r := evidence()
	r.Spec.Matrix[1].Backend = "gemini"
	r.Spec.Matrix[1].Model = "other"
	r.Table.Variants[0].MedianCostUSD = 2
	r.Table.Variants[2].MedianCostUSD = 2
	r.Table.Tasks[2].Passed = 10
	if got := FromEval(r, 0)["implement"]; got.Backend != "codex" || got.Model != "sol" || got.Effort != "high" {
		t.Fatalf("lower effort beat plan CLI: %+v", got)
	}
}

func TestFromEvalInvalidMarginReturnsEmptyTable(t *testing.T) {
	for _, margin := range []float64{-.01, 1.01, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got := FromEval(evidence(), margin); got == nil || len(got) != 0 {
			t.Errorf("margin %v: got %+v, want empty table", margin, got)
		}
	}
}

func TestFromEvalInheritsMeasuredTaskChoices(t *testing.T) {
	r := evidence()
	r.Spec.Backend = "api"
	r.Spec.Model = "spec-model"
	r.Spec.Effort = "max"
	r.Spec.Matrix = []eval.Variant{{Name: "high"}}
	r.Spec.Tasks[0].Backend = "codex"
	r.Spec.Tasks[0].Model = "task-model"
	r.Spec.Tasks[0].Effort = "medium"
	if got := FromEval(r, 0)["implement"]; got.Backend != "codex" || got.Model != "task-model" || got.Effort != "medium" {
		t.Fatalf("task choices lost: %+v", got)
	}
	if got := FromEval(r, 0)["review"]; got.Backend != "api" || got.Model != "spec-model" || got.Effort != "max" {
		t.Fatalf("spec choices lost: %+v", got)
	}
	// A single row cannot reproduce variants whose tasks in one purpose used
	// different models. Do not misattribute their combined pass rate.
	r.Spec.Tasks = append(r.Spec.Tasks, batch.Task{ID: "other", Prompt: "Implement another widget", Options: agent.Options{Model: "different"}})
	r.Table.Tasks = append(r.Table.Tasks, eval.TaskRow{Task: "other", Variant: "high", Passed: 10, Total: 10})
	if _, ok := FromEval(r, 0)["implement"]; ok {
		t.Fatal("mixed model evidence became one route")
	}
}

func TestApplyOnlyWhenAllChoicesEmpty(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	if err := Save(Table{"implement": {Backend: "codex", Model: "sol", Effort: "medium", Eval: "e1", Set: "2026-10-12"}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ backend, model, effort string }{{}, {backend: "claude"}, {model: "mine"}, {effort: "high"}} {
		b := tc.backend
		o := agent.Options{Model: tc.model, Effort: tc.effort}
		by, err := o.Route("", "Implement a widget", &b, os.Getenv("HRN_HOME"))
		if err != nil {
			t.Fatal(err)
		}
		if tc.backend == "" && tc.model == "" && tc.effort == "" {
			if b != "codex" || o.Model != "sol" || o.Effort != "medium" || by == nil || by.Category != "implement" || by.Eval != "e1" {
				t.Fatalf("routed: %s %+v %+v", b, o, by)
			}
		} else if by != nil || b != tc.backend || o.Model != tc.model || o.Effort != tc.effort {
			t.Fatalf("substituted: %s %+v %+v", b, o, by)
		}
	}
}

func TestApplyMissingUnknownAndBroken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	o := agent.Options{}
	b := ""
	if by, err := o.Route("", "Review patch", &b, home); by != nil || err != nil {
		t.Fatalf("missing: %v %v", by, err)
	}
	if err := Save(Table{"review": {Backend: "not-a-backend"}}); err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	prev := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = prev }()
	if by, err := o.Route("", "Review patch", &b, home); by != nil || err != nil || b != "" {
		t.Fatalf("unknown: %v %v %s", by, err, b)
	}
	data, _ := os.ReadFile(f.Name())
	if !strings.Contains(string(data), "warning:") || !strings.Contains(string(data), "not-a-backend") {
		t.Fatalf("warning: %s", data)
	}
	if err := os.WriteFile(filepath.Join(home, "route.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Route("", "Review patch", &b, home); err == nil {
		t.Fatal("broken table accepted")
	}
	b = "codex"
	if _, err := o.Route("", "Review patch", &b, home); err != nil {
		t.Fatalf("explicit choice read table: %v", err)
	}
}

func TestRouteSkipsARowWithAnInvalidEffortAndWarns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	if err := Save(Table{"review": {Backend: "codex", Effort: "enormous"}}); err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	prev := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = prev }()
	o, b := agent.Options{}, ""
	if by, err := o.Route("", "Review patch", &b, home); by != nil || err != nil || b != "" || o.Effort != "" {
		t.Fatalf("invalid row: %v %v %s %+v", by, err, b, o)
	}
	data, _ := os.ReadFile(f.Name())
	if !strings.Contains(string(data), "warning: ignoring route for review") || !strings.Contains(string(data), "enormous") {
		t.Fatalf("warning: %s", data)
	}
}

func TestRouteUsesADeclaredPurposeAndSaysSo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	if err := Save(Table{"implement": {Backend: "codex", Model: "sol"}, "review": {Backend: "claude", Model: "opus"}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ declared, prompt, want, backend string }{
		{"review", "Implement a widget", "review", "claude"},
		{"", "Purpose: review\nImplement a widget", "review", "claude"},
		{"", "Implement a widget", "implement", "codex"},
	} {
		o, b := agent.Options{}, ""
		by, err := o.Route(tc.declared, tc.prompt, &b, home)
		if err != nil || by == nil || by.Category != tc.want || b != tc.backend || by.Declared != (tc.want == "review") {
			t.Fatalf("%q %q: %+v %v %s", tc.declared, tc.prompt, by, err, b)
		}
	}
}

func TestFromEvalFindsAnUnnamedVariantByEvalsOwnName(t *testing.T) {
	r := evidence()
	r.Spec.Matrix = []eval.Variant{{Backend: "codex", Model: "sol", Effort: "low"}}
	name := r.Spec.Matrix[0].Key()
	r.Table.Variants = []eval.VariantRow{{Name: name, MedianCostUSD: 1}}
	r.Table.Tasks = []eval.TaskRow{{Task: "build", Variant: name, Passed: 1, Total: 1}}
	if row := FromEval(r, 0)["implement"]; row.Backend != "codex" || row.Effort != "low" {
		t.Fatalf("unnamed variant: %+v", row)
	}
}

func TestFromEvalDoesNotCreditAHarnessChangeToTheModel(t *testing.T) {
	for _, edit := range []func(*eval.Variant){
		func(v *eval.Variant) { v.PromptPrefix = "Read the tests first." },
		func(v *eval.Variant) { v.AppendSystemPrompt = "Run the tests." },
		func(v *eval.Variant) { v.NoSkills = true },
	} {
		r := evidence()
		edit(&r.Spec.Matrix[1]) // "low" wins implement without the change
		if got := FromEval(r, .05)["implement"]; got.Effort != "high" {
			t.Errorf("%+v: implement = %+v, want the plain variant", r.Spec.Matrix[1], got)
		}
	}
	// A harness variant that beats every plain one does not set the bar either.
	r := evidence()
	r.Spec.Matrix[0].NoSkills = true // "high", the only one at 10/10 on check
	if got := FromEval(r, .05)["review"]; got.Effort != "low" || got.Model != "sol" {
		t.Errorf("review = %+v, want the best plain variant", got)
	}
	r = evidence()
	r.Spec.AppendSystemPrompt = "Run the tests."
	if got := FromEval(r, .05); len(got) != 0 {
		t.Errorf("a spec-wide harness change routes nothing: %+v", got)
	}
	r = evidence()
	r.Spec.Tasks[0].NoSkills = true
	if got := FromEval(r, .05); len(got) != 1 || got["implement"].Backend != "" {
		t.Errorf("the task with a harness change is left out: %+v", got)
	}
}

func TestFromEvalSkipsNonDefaultVariantOptions(t *testing.T) {
	for name, edit := range map[string]func(*eval.Variant){
		"lean":        func(v *eval.Variant) { v.Lean = agent.Bool(false) },
		"shape_tools": func(v *eval.Variant) { v.ShapeTools = true },
		"retrieval":   func(v *eval.Variant) { v.Retrieval = "required" },
	} {
		t.Run(name, func(t *testing.T) {
			r := evidence()
			edit(&r.Spec.Matrix[1])
			if got := FromEval(r, .05)["implement"]; got.Effort != "high" {
				t.Fatalf("selected non-default variant: %+v", got)
			}
		})
	}
	r := evidence()
	r.Spec.Matrix[0].Lean = agent.Bool(false)
	if got := FromEval(r, .05)["review"]; got.Effort != "low" {
		t.Fatalf("non-default variant set the pass-rate bar: %+v", got)
	}
	r = evidence()
	r.Spec.Matrix[1].Lean = agent.Bool(true)
	r.Spec.Matrix[1].Retrieval = "off"
	if got := FromEval(r, .05)["implement"]; got.Effort != "low" {
		t.Fatalf("explicit defaults skipped: %+v", got)
	}
	for name, edit := range map[string]func(*agent.Options){
		"lean":      func(o *agent.Options) { o.Lean = agent.Bool(false) },
		"retrieval": func(o *agent.Options) { o.Retrieval = "required" },
	} {
		t.Run("task "+name, func(t *testing.T) {
			r := evidence()
			edit(&r.Spec.Tasks[0].Options)
			if _, ok := FromEval(r, .05)["implement"]; ok {
				t.Fatal("non-default task options routed")
			}
		})
		t.Run("spec "+name, func(t *testing.T) {
			r := evidence()
			edit(&r.Spec.Options)
			if got := FromEval(r, .05); len(got) != 0 {
				t.Fatalf("non-default spec options routed: %+v", got)
			}
		})
	}
}

func TestFromEvalLeavesCellsThatNeverActedOutOfThePassRate(t *testing.T) {
	r := evidence()
	r.Table.Tasks[2].DidNotAct = 1 // small's one miss on build never acted: 9 of 9
	if got := FromEval(r, .05)["implement"]; got.Model != "small" {
		t.Fatalf("implement = %+v; the cheapest variant passed every cell that acted", got)
	}
	r.Table.Tasks[5] = eval.TaskRow{Task: "check", Variant: "small", Total: 10, DidNotAct: 10}
	r.Table.Tasks[4].Passed = 0
	r.Table.Tasks[3].Passed = 0
	if got, ok := FromEval(r, .05)["review"]; ok && got.Model == "small" {
		t.Fatalf("review = %+v; a variant with no cell that acted is no evidence", got)
	}
}

func TestFromEvalLeavesOutCellsThatDidNotStart(t *testing.T) {
	r := evidence()
	// Every cell of the cheapest variant never started; the old counts still made it the winner.
	for i := range r.Table.Tasks {
		if r.Table.Tasks[i].Variant == "small" {
			r.Table.Tasks[i].DidNotStart = r.Table.Tasks[i].Total
		}
	}
	for i := range r.Table.Variants {
		if r.Table.Variants[i].Name == "small" {
			r.Table.Variants[i].DidNotStart = r.Table.Variants[i].Attempts
		}
	}
	tab := FromEval(r, .1)
	if got := tab["implement"]; got.Model == "small" {
		t.Fatalf("a variant whose cells never started was still chosen: %+v", got)
	}
}
