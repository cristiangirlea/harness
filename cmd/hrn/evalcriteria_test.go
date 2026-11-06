package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/eval"
)

func TestEvalPrintsCriterionCountsAndDiffsOnlyWhenPresent(t *testing.T) {
	a, b := diffPair()
	for _, present := range []bool{false, true} {
		if present {
			a.Table.Variants[0].Criteria = []eval.CriterionSummary{{ID: "safe", Met: 1, Scored: 2}}
			b.Table.Variants[0].Criteria = []eval.CriterionSummary{{ID: "safe", Met: 3, Scored: 4}, {ID: "new", Scored: 4}}
		}
		var out bytes.Buffer
		r := newRenderer(false)
		r.out = &out
		printEvalDiff(r, eval.Compare(a, b))
		if strings.Contains(out.String(), "criterion") != present {
			t.Fatalf("criterion table presence: %s", out.String())
		}
		if present {
			for _, want := range []string{"safe", arrow("1/2", "3/4"), arrow("-", "0/4")} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("missing %q: %s", want, out.String())
				}
			}
			out.Reset()
			printEval(r, a)
			if !strings.Contains(out.String(), "safe: 1/2") {
				t.Fatalf("missing criterion count: %s", out.String())
			}
		}
	}
}

func TestEvalPrintsCriterionDisagreementsOnlyForRepeatedJudges(t *testing.T) {
	a, _ := diffPair()
	for _, tc := range []struct {
		rowRepeats, specRepeats int
		show                    bool
	}{{0, 0, false}, {2, 0, true}, {0, 2, true}} {
		a.Table.Variants[0].JudgeRepeats = tc.rowRepeats
		a.Spec.Judge = &eval.Judge{Repeats: tc.specRepeats}
		a.Table.Variants[0].Criteria = []eval.CriterionSummary{{ID: "safe", Met: 1, Scored: 2, Disagreed: 1}, {ID: "same", Scored: 2}}
		var out bytes.Buffer
		r := newRenderer(false)
		r.out = &out
		printEval(r, a)
		if strings.Contains(out.String(), "safe: 1/2 met, 1 disagreed") != tc.show || strings.Contains(out.String(), "0 disagreed") {
			t.Fatalf("disagreements: %s", out.String())
		}
	}
}

func TestEvalDiffPrintsCriterionDisagreementCountsAndOneSidedCriteria(t *testing.T) {
	for _, tc := range []struct {
		name               string
		repeats, disagreed int
		onlyB              bool
	}{
		{"repeats", 2, 0, false}, {"disagreements", 0, 1, false}, {"only B", 2, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := diffPair()
			if !tc.onlyB {
				a.Table.Variants[0].Criteria = []eval.CriterionSummary{{ID: "safe", Met: 1, Scored: 2}}
			}
			b.Table.Variants[0].Criteria = []eval.CriterionSummary{{ID: "safe", Met: 3, Scored: 4, Disagreed: tc.disagreed}}
			b.Table.Variants[0].JudgeRepeats = tc.repeats
			var out bytes.Buffer
			r := newRenderer(false)
			r.out = &out
			printEvalDiff(r, eval.Compare(a, b))
			before, metBefore := "-", "1/2"
			if tc.onlyB {
				before, metBefore = "-", "-"
			}
			after := "-"
			if tc.repeats > 1 {
				after = fmt.Sprint(tc.disagreed)
			}
			for _, want := range []string{"criterion", "disagreed", arrow(metBefore, "3/4"), arrow(before, after)} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("missing %q: %s", want, out.String())
				}
			}
		})
	}
}
