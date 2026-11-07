package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/eval"
	"github.com/cristiangirlea/harness/internal/kernel"
	"github.com/cristiangirlea/harness/internal/report"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestEvalPrintsTenToolsByCallsAndCountsTheRest(t *testing.T) {
	stats := map[string]kernel.ToolStat{}
	for i := 0; i < 12; i++ {
		stats[fmt.Sprintf("tool%02d", i)] = kernel.ToolStat{Calls: i + 1, Errors: i, ResultBytes: 100 + i}
	}
	rec := eval.Record{Table: eval.Table{Variants: []eval.VariantRow{{Name: "v", ToolStats: stats, AttemptsWithToolStats: 2}}}}
	var buf bytes.Buffer
	r := newRenderer(false)
	r.out = &buf
	printEval(r, rec)
	out := buf.String()
	for _, want := range []string{"tool stats: v", "2 attempts", "calls", "errors", "bytes", "tool11", "tool02", "(2 more tools)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q: %s", want, out)
		}
	}
	if strings.Contains(out, "tool01") || strings.Contains(out, "tool00") || strings.Index(out, "tool11") > strings.Index(out, "tool02") {
		t.Fatalf("unexpected tools or order: %s", out)
	}
	if len(stats) != 12 {
		t.Fatal("display truncated stored stats")
	}
	buf.Reset()
	printEval(r, eval.Record{})
	if strings.Contains(buf.String(), "tool stats:") {
		t.Fatal(buf.String())
	}
}

func TestEvalDiffPrintsToolStatsOnlyWhenEitherSideHasThem(t *testing.T) {
	for _, sides := range []string{"neither", "a", "b", "both"} {
		t.Run(sides, func(t *testing.T) {
			a, b := diffPair()
			a.Table.Variants[0].AttemptsWithToolStats = 3
			b.Table.Variants[0].AttemptsWithToolStats = 5
			if sides == "a" || sides == "both" {
				a.Table.Variants[0].ToolStats = map[string]kernel.ToolStat{"edit_file": {Calls: 4, Errors: 2, ResultBytes: 90}}
			}
			if sides == "b" || sides == "both" {
				b.Table.Variants[0].ToolStats = map[string]kernel.ToolStat{"edit_file": {Calls: 3, ResultBytes: 70}}
			}
			var buf bytes.Buffer
			r := newRenderer(false)
			r.out = &buf
			printEvalDiff(r, eval.Compare(a, b))
			out := buf.String()
			if strings.Contains(out, "tool stats:") != (sides != "neither") {
				t.Fatal(out)
			}
			if sides != "neither" {
				if !strings.Contains(out, "sonnet (aaaaa: 3 attempts -> bbbbb: 5 attempts)") {
					t.Fatal(out)
				}
				lines := strings.Split(out, "\n")
				for i, line := range lines {
					if strings.HasPrefix(strings.TrimSpace(line), "task ") && (i == 0 || strings.TrimSpace(lines[i-1]) != "") {
						t.Fatal("missing blank line before task header: " + out)
					}
				}
				for _, want := range []string{"edit_file", "calls", "errors", "bytes"} {
					if !strings.Contains(out, want) {
						t.Errorf("missing %q: %s", want, out)
					}
				}
			}
			if sides == "both" {
				for _, want := range []string{arrow("4", "3"), arrow("2", "0"), arrow("90", "70")} {
					if !strings.Contains(out, want) {
						t.Errorf("missing %q: %s", want, out)
					}
				}
			}
		})
	}
}

func TestReportJSONCarriesToolTotalsAcrossRuns(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, stats := range []map[string]kernel.ToolStat{
		{"read_file": {Calls: 2, ResultBytes: 8}},
		{"read_file": {Calls: 1, Errors: 1, ResultBytes: 3}, "edit_file": {Calls: 1}},
		nil,
	} {
		w, err := runlog.Create("fake", agent.Task{Prompt: "p"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Close(agent.Result{Status: agent.StatusOK, ToolStats: stats}); err != nil {
			t.Fatal(err)
		}
	}
	out, err := captureStdout(t, func() error { return cmdReport([]string{"--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var rep report.Report
	if err := json.Unmarshal(out, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.ToolStats["read_file"] != (kernel.ToolStat{Calls: 3, Errors: 1, ResultBytes: 11}) || rep.ToolStats["edit_file"].Calls != 1 {
		t.Fatalf("report = %s", out)
	}
}

func TestEvalDiffCapsTheCombinedToolTableAndPreservesUnknownSides(t *testing.T) {
	a, b := diffPair()
	a.Table.Variants[0].ToolStats = map[string]kernel.ToolStat{}
	b.Table.Variants[0].ToolStats = map[string]kernel.ToolStat{}
	for i := 0; i < 12; i++ {
		a.Table.Variants[0].ToolStats[fmt.Sprintf("tool%02d", i)] = kernel.ToolStat{Calls: 1}
		b.Table.Variants[0].ToolStats[fmt.Sprintf("tool%02d", i)] = kernel.ToolStat{Calls: i + 1}
	}
	var buf bytes.Buffer
	r := newRenderer(false)
	r.out = &buf
	printEvalDiff(r, eval.Compare(a, b))
	out := buf.String()
	if !strings.Contains(out, "(2 more tools)") || !strings.Contains(out, "tool11") || strings.Contains(out, "tool01") || strings.Index(out, "tool11") > strings.Index(out, "tool02") {
		t.Fatal(out)
	}
	b.Table.Variants[0].ToolStats = nil
	buf.Reset()
	printEvalDiff(r, eval.Compare(a, b))
	if !strings.Contains(buf.String(), arrow("1", "-")) {
		t.Fatal(buf.String())
	}
	data, err := json.Marshal(eval.Compare(a, b))
	if err != nil || !strings.Contains(string(data), "tool11") || !strings.Contains(string(data), "tool00") {
		t.Fatalf("diff = %s, error = %v", data, err)
	}
}
