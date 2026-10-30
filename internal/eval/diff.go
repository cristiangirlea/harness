package eval

import (
	"encoding/json"
	"math"
	"slices"

	"github.com/cristiangirlea/harness/internal/agent"
)

// Diff matches named eval rows, retaining unmatched rows with a nil side.
type Diff struct {
	A string `json:"a"` // the first eval's id
	B string `json:"b"`

	Variants []VariantPair `json:"variants"`
	Tasks    []TaskPair    `json:"tasks"`

	// PromptChanged marks tasks whose scores answer different prompts.
	PromptChanged []string `json:"prompt_changed,omitempty"`
	// VariantChanged flags backend/model/effort/harness changes under the same name.
	VariantChanged []string `json:"variant_changed,omitempty"`
	// GateChanged flags gates or expect patterns that change passing's meaning.
	GateChanged []string `json:"gate_changed,omitempty"`
	// TaskOptionsChanged flags changes to a task's own run options.
	TaskOptionsChanged []string `json:"task_options_changed,omitempty"`
	// JudgeChanged ignores repeats, which leave the scoring scale unchanged.
	JudgeChanged bool `json:"judge_changed,omitempty"`
	Judged       bool `json:"judged,omitempty"`
	// SetupChanged excludes the variant's own harness options.
	SetupChanged []SetupChange `json:"setup_changed,omitempty"`
}

// SetupChange is what differs in one variant's setup between the two evals.
type SetupChange struct {
	Variant string `json:"variant"`
	// Files lists additions, removals and edits; other lists contain names unique to one side.
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
	// WithinNoise means the score moved less than either measured judge spread.
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

// Compare matches rows in a's order, then appends b-only rows in b's order.
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

	// Expose confounders to avoid misattributing score changes.
	type check struct {
		prompt                                                   string
		gates                                                    []string
		expect                                                   string
		model, effort, systemPrompt, retrieval, retrievalVersion string
		noSkills                                                 bool
	}
	before := map[string]check{}
	for _, t := range a.Spec.Tasks {
		before[t.ID] = check{t.Prompt, firstOfList(t.Gate, a.Spec.Gate), t.Expect, t.Model, t.Effort, t.AppendSystemPrompt, t.Retrieval, t.RetrievalVersion, t.NoSkills}
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
		if was.model != t.Model || was.effort != t.Effort || was.systemPrompt != t.AppendSystemPrompt || was.noSkills != t.NoSkills || was.retrieval != t.Retrieval || was.retrievalVersion != t.RetrievalVersion {
			d.TaskOptionsChanged = append(d.TaskOptionsChanged, t.ID)
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
		// Backend-specific files belong to the variant, even for inherited backends.
		if v.A.Setup.Backend != v.B.Setup.Backend {
			if !slices.Contains(d.VariantChanged, v.Name) {
				d.VariantChanged = append(d.VariantChanged, v.Name)
			}
			continue
		}
		// Different tasks may deliberately use different MCP configs.
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

// Repeat counts measure noise without changing the scoring scale.
func judgeScale(j *Judge) []byte {
	if j != nil {
		c := *j
		c.Repeats = 0
		j = &c
	}
	out, _ := json.Marshal(j)
	return out
}

// Without repeats, neither side can establish judge noise.
func withinNoise(a, b judgeScore) bool {
	if a.judged == 0 || b.judged == 0 {
		return false
	}
	sa, measuredA := a.spread()
	sb, measuredB := b.spread()
	return (measuredA || measuredB) && math.Abs(b.mean-a.mean) < max(sa, sb)
}

func firstOfList(own, spec []string) []string {
	if len(own) > 0 {
		return own
	}
	return spec
}

// Unset differs from an explicit false as well as true.
func sameOptional(a, b *bool) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
