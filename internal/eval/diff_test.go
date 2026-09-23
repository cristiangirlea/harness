package eval

import (
	"testing"

	"github.com/cristiangirlea/harness/internal/batch"
)

// twoEvals builds a before and an after of the same task set: sonnet improved,
// haiku was dropped, opus was added, and the greet prompt was reworded.
func twoEvals() (Record, Record) {
	before := Record{ID: "aaaaa",
		Spec: Spec{Spec: batch.Spec{Tasks: []batch.Task{{ID: "greet", Prompt: "say hi"}, {ID: "vet", Prompt: "run vet"}}}},
		Table: Table{
			Variants: []VariantRow{
				{Name: "sonnet", Attempts: 4, Passed: 2, MedianCostUSD: 0.15, Judged: 2, MeanScore: 6},
				{Name: "haiku", Attempts: 4, Passed: 4, MedianCostUSD: 0.05},
			},
			Tasks: []TaskRow{
				{Task: "greet", Variant: "sonnet", Passed: 1, Total: 2},
				{Task: "greet", Variant: "haiku", Passed: 2, Total: 2},
				{Task: "vet", Variant: "sonnet", Passed: 1, Total: 2},
			},
		}}
	after := Record{ID: "bbbbb",
		Spec: Spec{Spec: batch.Spec{Tasks: []batch.Task{{ID: "greet", Prompt: "say hello"}, {ID: "vet", Prompt: "run vet"}}}},
		Table: Table{
			Variants: []VariantRow{
				{Name: "opus", Attempts: 2, Passed: 2, MedianCostUSD: 0.40},
				{Name: "sonnet", Attempts: 4, Passed: 4, MedianCostUSD: 0.12, Judged: 4, MeanScore: 8.5},
			},
			Tasks: []TaskRow{
				{Task: "greet", Variant: "sonnet", Passed: 2, Total: 2},
				{Task: "greet", Variant: "opus", Passed: 1, Total: 1},
				{Task: "vet", Variant: "sonnet", Passed: 2, Total: 2},
			},
		}}
	return before, after
}

func TestCompareMatchesVariantsAndTasksByName(t *testing.T) {
	a, b := twoEvals()
	d := Compare(a, b)
	if d.A != "aaaaa" || d.B != "bbbbb" {
		t.Errorf("ids = %s, %s", d.A, d.B)
	}

	// The first eval's order, then what only the second has.
	var names []string
	for _, v := range d.Variants {
		names = append(names, v.Name)
	}
	if got := join(names); got != "sonnet,haiku,opus" {
		t.Fatalf("variants = %s", got)
	}
	sonnet, haiku, opus := d.Variants[0], d.Variants[1], d.Variants[2]
	if sonnet.A == nil || sonnet.B == nil || sonnet.A.Passed != 2 || sonnet.B.Passed != 4 {
		t.Errorf("sonnet = %+v", sonnet)
	}
	if haiku.A == nil || haiku.B != nil {
		t.Errorf("haiku is only in the first eval: %+v", haiku)
	}
	if opus.A != nil || opus.B == nil {
		t.Errorf("opus is only in the second eval: %+v", opus)
	}

	var cells []string
	for _, c := range d.Tasks {
		side := ""
		if c.A != nil {
			side += "a"
		}
		if c.B != nil {
			side += "b"
		}
		cells = append(cells, c.Task+"/"+c.Variant+":"+side)
	}
	if got := join(cells); got != "greet/sonnet:ab,greet/haiku:a,vet/sonnet:ab,greet/opus:b" {
		t.Errorf("task cells = %s", got)
	}

	if got := join(d.PromptChanged); got != "greet" {
		t.Errorf("prompt changed = %q, want only greet", got)
	}
	if !d.Judged {
		t.Error("either side was judged, so the diff has a score column")
	}
}

func TestCompareAnEvalWithItself(t *testing.T) {
	a, _ := twoEvals()
	d := Compare(a, a)
	if len(d.PromptChanged) != 0 {
		t.Errorf("nothing changed: %v", d.PromptChanged)
	}
	for _, v := range d.Variants {
		if v.A == nil || v.B == nil {
			t.Errorf("%s is on both sides", v.Name)
		}
	}
}

func TestCompareWithoutAJudge(t *testing.T) {
	a, b := twoEvals()
	for _, r := range []*Record{&a, &b} {
		for i := range r.Table.Variants {
			r.Table.Variants[i].Judged = 0
		}
	}
	if Compare(a, b).Judged {
		t.Error("no side was judged, so there is no score column")
	}
}

func join(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += ","
		}
		out += x
	}
	return out
}

func TestCompareNotesWhatElseChanged(t *testing.T) {
	a, b := twoEvals()
	a.Spec.Matrix = []Variant{{Name: "sonnet", Backend: "claude", Model: "sonnet"}, {Name: "haiku", Backend: "claude", Model: "haiku"}}
	// Same name, different model: not the same variant any more.
	b.Spec.Matrix = []Variant{{Name: "opus", Backend: "claude", Model: "opus"}, {Name: "sonnet", Backend: "api", Model: "claude-sonnet-5"}}
	a.Spec.Gate = []string{"go test ./..."}
	b.Spec.Gate = []string{"go test ./..."}
	b.Spec.Tasks[1].Gate = []string{"true"} // vet's gate got easier
	a.Spec.Judge = &Judge{Rubric: "strict"}
	b.Spec.Judge = &Judge{Rubric: "lenient"}

	d := Compare(a, b)
	if got := join(d.VariantChanged); got != "sonnet" {
		t.Errorf("variant changed = %q, want sonnet", got)
	}
	if got := join(d.GateChanged); got != "vet" {
		t.Errorf("gate changed = %q, want vet", got)
	}
	if !d.JudgeChanged {
		t.Error("the rubric changed")
	}

	same := Compare(a, a)
	if len(same.VariantChanged)+len(same.GateChanged) != 0 || same.JudgeChanged {
		t.Errorf("an eval against itself changed nothing: %+v", same)
	}
}
