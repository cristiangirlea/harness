package eval

import (
	"encoding/json"
	"slices"
)

// Diff sets two evals side by side: the question it answers is "did this
// model, prompt or harness change help". Rows are matched by name, and a row
// present on one side only is kept with the other side nil rather than
// dropped, so a variant that was added or removed is visible.
type Diff struct {
	A string `json:"a"` // the first eval's id
	B string `json:"b"`

	Variants []VariantPair `json:"variants"`
	Tasks    []TaskPair    `json:"tasks"`

	// PromptChanged lists the task ids whose prompt differs between the two
	// specs: often the change being measured, but those rows do not compare
	// like with like.
	PromptChanged []string `json:"prompt_changed,omitempty"`
	// VariantChanged lists the variant names that run something else in b:
	// another backend, model or effort under the same name.
	VariantChanged []string `json:"variant_changed,omitempty"`
	// GateChanged lists the task ids whose gates or expect pattern differ, so
	// passing means something else on each side.
	GateChanged []string `json:"gate_changed,omitempty"`
	// JudgeChanged is true when the judge or its rubric differs.
	JudgeChanged bool `json:"judge_changed,omitempty"`
	// Judged is true when either eval has judge scores to compare.
	Judged bool `json:"judged,omitempty"`
}

// VariantPair is one variant in both evals; a nil side did not run it.
type VariantPair struct {
	Name string      `json:"name"`
	A    *VariantRow `json:"a,omitempty"`
	B    *VariantRow `json:"b,omitempty"`
}

// TaskPair is one task under one variant in both evals.
type TaskPair struct {
	Task    string   `json:"task"`
	Variant string   `json:"variant"`
	A       *TaskRow `json:"a,omitempty"`
	B       *TaskRow `json:"b,omitempty"`
}

// Compare lines up a and b. Rows come in a's order, then whatever only b has,
// in b's order.
func Compare(a, b Record) Diff {
	d := Diff{A: a.ID, B: b.ID}

	variants := map[string]int{}
	for i := range a.Table.Variants {
		v := &a.Table.Variants[i]
		variants[v.Name] = len(d.Variants)
		d.Variants = append(d.Variants, VariantPair{Name: v.Name, A: v})
	}
	for i := range b.Table.Variants {
		v := &b.Table.Variants[i]
		if j, ok := variants[v.Name]; ok {
			d.Variants[j].B = v
		} else {
			d.Variants = append(d.Variants, VariantPair{Name: v.Name, B: v})
		}
	}
	for _, v := range d.Variants {
		d.Judged = d.Judged || (v.A != nil && v.A.Judged > 0) || (v.B != nil && v.B.Judged > 0)
	}

	type key struct{ task, variant string }
	cells := map[key]int{}
	for i := range a.Table.Tasks {
		t := &a.Table.Tasks[i]
		cells[key{t.Task, t.Variant}] = len(d.Tasks)
		d.Tasks = append(d.Tasks, TaskPair{Task: t.Task, Variant: t.Variant, A: t})
	}
	for i := range b.Table.Tasks {
		t := &b.Table.Tasks[i]
		if j, ok := cells[key{t.Task, t.Variant}]; ok {
			d.Tasks[j].B = t
		} else {
			d.Tasks = append(d.Tasks, TaskPair{Task: t.Task, Variant: t.Variant, B: t})
		}
	}

	// What else differs between the specs, so a moved number is not credited
	// to the wrong change.
	type check struct {
		prompt string
		gates  []string
		expect string
	}
	before := map[string]check{}
	for _, t := range a.Spec.Tasks {
		before[t.ID] = check{t.Prompt, firstOfList(t.Gate, a.Spec.Gate), t.Expect}
	}
	for _, t := range b.Spec.Tasks {
		was, ok := before[t.ID]
		if !ok {
			continue
		}
		if was.prompt != t.Prompt {
			d.PromptChanged = append(d.PromptChanged, t.ID)
		}
		if !slices.Equal(was.gates, firstOfList(t.Gate, b.Spec.Gate)) || was.expect != t.Expect {
			d.GateChanged = append(d.GateChanged, t.ID)
		}
	}
	defs := map[string]Variant{}
	for _, v := range a.Spec.Matrix {
		defs[v.name()] = v
	}
	for _, v := range b.Spec.Matrix {
		if was, ok := defs[v.name()]; ok && (was.Backend != v.Backend || was.Model != v.Model || was.Effort != v.Effort) {
			d.VariantChanged = append(d.VariantChanged, v.name())
		}
	}
	ja, _ := json.Marshal(a.Spec.Judge)
	jb, _ := json.Marshal(b.Spec.Judge)
	d.JudgeChanged = string(ja) != string(jb)
	return d
}

// firstOfList is a task's own list when it has one, else the spec's.
func firstOfList(own, spec []string) []string {
	if len(own) > 0 {
		return own
	}
	return spec
}
