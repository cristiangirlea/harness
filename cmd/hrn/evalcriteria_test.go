package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/eval"
	"github.com/cristiangirlea/harness/internal/kernel"
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
		after              string
	}{
		{"repeats", 2, 0, false, "0"}, {"only B", 2, 1, true, "1"},
		// Repeats with an errored verdict leave JudgeRepeats unset, yet a counted disagreement was measured.
		{"disagreements without full repeats", 0, 1, false, "1"},
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
			for _, want := range []string{"criterion", "disagreed", arrow(metBefore, "3/4"), arrow(before, tc.after)} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("missing %q: %s", want, out.String())
				}
			}
		})
	}
}

func TestEvalDiffWithToolStatsAndCriteriaPrintsEachTableAfterABlankLine(t *testing.T) {
	a, b := diffPair()
	for _, rec := range []*eval.Record{&a, &b} {
		rec.Table.Variants[0].AttemptsWithToolStats = 2
		rec.Table.Variants[0].ToolStats = map[string]kernel.ToolStat{"edit_file": {Calls: 2, Errors: 1, ResultBytes: 40}}
		rec.Table.Variants[0].Criteria = []eval.CriterionSummary{{ID: "safe", Met: 1, Scored: 2}}
	}
	var buf bytes.Buffer
	r := newRenderer(false)
	r.out = &buf
	printEvalDiff(r, eval.Compare(a, b))
	lines := strings.Split(buf.String(), "\n")
	at := map[string]int{}
	for i, line := range lines {
		head := ""
		switch f := strings.Fields(line); {
		case len(f) > 1 && f[0] == "tool" && f[1] == "stats:":
			head = "tools"
		case len(f) > 1 && f[0] == "variant" && f[1] == "criterion":
			head = "criteria"
		case len(f) > 0 && f[0] == "task":
			head = "tasks"
		}
		if _, seen := at[head]; head != "" && !seen {
			at[head] = i
			if i == 0 || strings.TrimSpace(lines[i-1]) != "" {
				t.Errorf("no blank line before the %s table:\n%s", head, buf.String())
			}
		}
	}
	if len(at) != 3 || !(at["tools"] < at["criteria"] && at["criteria"] < at["tasks"]) {
		t.Fatalf("tables out of order %v:\n%s", at, buf.String())
	}
}

func TestEvalOutputStripsControlCharactersFromToolNamesAndCriterionIDs(t *testing.T) {
	const hostile = "x\x1b]8;;http://evil\x07y\x1b[2J"
	a, b := diffPair()
	for _, rec := range []*eval.Record{&a, &b} {
		rec.Table.Variants[0].Criteria = []eval.CriterionSummary{{ID: hostile, Met: 1, Scored: 2}}
		rec.Table.Variants[0].ToolStats = map[string]kernel.ToolStat{hostile: {Calls: 1}}
		rec.Table.Variants[0].AttemptsWithToolStats = 1
	}
	var out bytes.Buffer
	r := newRenderer(false)
	r.out = &out
	printEval(r, a)
	printEvalDiff(r, eval.Compare(a, b))
	if strings.ContainsAny(out.String(), "\x1b\x07") {
		t.Fatalf("raw control characters reached the terminal: %q", out.String())
	}
	if strings.Count(out.String(), "x�]8;;http://evil�y�[2J") < 4 {
		t.Fatalf("names were not shown in printable form: %q", out.String())
	}
}
