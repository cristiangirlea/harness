package eval

import (
	"encoding/json"
	"math"
	"slices"

	"github.com/cristiangirlea/harness/internal/agent"
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
	// another backend, model, effort or harness under the same name.
	VariantChanged []string `json:"variant_changed,omitempty"`
	// GateChanged lists the task ids whose gates or expect pattern differ, so
	// passing means something else on each side.
	GateChanged []string `json:"gate_changed,omitempty"`
	// JudgeChanged is true when the judge or its rubric differs; its repeats
	// do not change the scale.
	JudgeChanged bool `json:"judge_changed,omitempty"`
	// Judged is true when either eval has judge scores to compare.
	Judged bool `json:"judged,omitempty"`
	// SetupChanged lists the variants whose runs started with another agent
	// setup in b beyond the variant's own harness options.
	SetupChanged []SetupChange `json:"setup_changed,omitempty"`
}

// SetupChange is what differs in one variant's setup between the two evals.
type SetupChange struct {
	Variant string `json:"variant"`
	// Files were added, removed or changed; the names below are on one side only.
	Files   []string `json:"files,omitempty"`
	MCP     []string `json:"mcp,omitempty"`
	Plugins []string `json:"plugins,omitempty"`
	Skills  []string `json:"skills,omitempty"`
}

// VariantPair is one variant in both evals; a nil side did not run it.
type VariantPair struct {
	Name string      `json:"name"`
	A    *VariantRow `json:"a,omitempty"`
	B    *VariantRow `json:"b,omitempty"`
	// WithinNoise: the judge score moved less than the judge's own spread
	// between repeats, on either side.
	WithinNoise bool `json:"within_noise,omitempty"`
}

// TaskPair is one task under one variant in both evals.
type TaskPair struct {
	Task    string   `json:"task"`
	Variant string   `json:"variant"`
	A       *TaskRow `json:"a,omitempty"`
	B       *TaskRow `json:"b,omitempty"`
	// WithinNoise is as in VariantPair, for this task alone.
	WithinNoise bool `json:"within_noise,omitempty"`
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
	for i, v := range d.Variants {
		d.Judged = d.Judged || (v.A != nil && v.A.Judged > 0) || (v.B != nil && v.B.Judged > 0)
		if v.A != nil && v.B != nil {
			d.Variants[i].WithinNoise = withinNoise(v.A.judge(), v.B.judge())
		}
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

	for i, t := range d.Tasks {
		if t.A != nil && t.B != nil {
			d.Tasks[i].WithinNoise = withinNoise(t.A.judge(), t.B.judge())
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
		defs[v.Key()] = v
	}
	for _, v := range b.Spec.Matrix {
		if was, ok := defs[v.Key()]; ok && (was.Backend != v.Backend || was.Model != v.Model || was.Effort != v.Effort ||
			was.PromptPrefix != v.PromptPrefix || was.AppendSystemPrompt != v.AppendSystemPrompt || was.NoSkills != v.NoSkills ||
			!sameOptional(was.Lean, v.Lean) || was.ShapeTools != v.ShapeTools || was.Retrieval != v.Retrieval || was.RetrievalVersion != v.RetrievalVersion) {
			d.VariantChanged = append(d.VariantChanged, v.Key())
		}
	}
	for _, v := range d.Variants {
		if v.A == nil || v.B == nil || v.A.Setup == nil || v.B.Setup == nil {
			continue
		}
		// Another backend loads other files by design: that is a variant change,
		// even when the backend came from the spec, a task or the default.
		if v.A.Setup.Backend != v.B.Setup.Backend {
			if !slices.Contains(d.VariantChanged, v.Name) {
				d.VariantChanged = append(d.VariantChanged, v.Name)
			}
			continue
		}
		// Setups of two different tasks may differ by design (a task's own MCP configs).
		if v.A.SetupTask == v.B.SetupTask && v.A.Setup.BaseDigest != v.B.Setup.BaseDigest {
			d.SetupChanged = append(d.SetupChanged, setupChange(v.Name, v.A.Setup, v.B.Setup))
		}
	}
	d.JudgeChanged = string(judgeScale(a.Spec.Judge)) != string(judgeScale(b.Spec.Judge))
	return d
}

func setupChange(variant string, a, b *agent.Setup) SetupChange {
	c := SetupChange{Variant: variant, MCP: oneSide(a.MCP, b.MCP), Plugins: oneSide(a.Plugins, b.Plugins), Skills: oneSide(a.Skills, b.Skills)}
	for name, hash := range a.Files {
		if b.Files[name] != hash {
			c.Files = append(c.Files, name)
		}
	}
	for name := range b.Files {
		if _, ok := a.Files[name]; !ok {
			c.Files = append(c.Files, name)
		}
	}
	slices.Sort(c.Files)
	return c
}

// oneSide is the names in a or b but not both, sorted.
func oneSide(a, b []string) []string {
	var out []string
	for _, n := range a {
		if !slices.Contains(b, n) {
			out = append(out, n)
		}
	}
	for _, n := range b {
		if !slices.Contains(a, n) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// judgeScale is the judge as JSON, without what does not change its scores' scale.
func judgeScale(j *Judge) []byte {
	if j != nil {
		c := *j
		c.Repeats = 0
		j = &c
	}
	out, _ := json.Marshal(j)
	return out
}

// withinNoise reports a judge-score change smaller than the judge's own
// spread; with no repeats on either side there is no spread to compare.
func withinNoise(a, b judgeScore) bool {
	if a.judged == 0 || b.judged == 0 {
		return false
	}
	sa, measuredA := a.spread()
	sb, measuredB := b.spread()
	return (measuredA || measuredB) && math.Abs(b.mean-a.mean) < max(sa, sb)
}

// firstOfList is a task's own list when it has one, else the spec's.
func firstOfList(own, spec []string) []string {
	if len(own) > 0 {
		return own
	}
	return spec
}

// sameOptional compares two optional settings, unset differing from either value.
func sameOptional(a, b *bool) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
