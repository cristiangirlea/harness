package eval

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

	prompts := map[string]string{}
	for _, t := range a.Spec.Tasks {
		prompts[t.ID] = t.Prompt
	}
	for _, t := range b.Spec.Tasks {
		if p, ok := prompts[t.ID]; ok && p != t.Prompt {
			d.PromptChanged = append(d.PromptChanged, t.ID)
		}
	}
	return d
}
