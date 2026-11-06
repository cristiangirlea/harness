package main

import (
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/eval"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestShowPrintsEachRefusalFlagAfterTheResult(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := runlog.Create("fake", agent.Task{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{Status: agent.StatusOK, Final: "finished", RefusalFlags: []agent.RefusalFlag{{Kind: "written", Rule: "git-push", Refused: "git push", Later: "write_file send.sh"}}}); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return cmdShow([]string{w.ID()}) })
	if err != nil {
		t.Fatal(err)
	}
	want := "refusal flag (written): refused git-push: git push ... later write_file send.sh"
	if !strings.Contains(string(out), want) || strings.Index(string(out), want) < strings.Index(string(out), "finished") {
		t.Fatalf("output = %s", out)
	}
}

func TestEvalPrintsARefusalFlagsColumnOnlyWhenNeeded(t *testing.T) {
	for _, count := range []int{0, 3} {
		rec := eval.Record{Table: eval.Table{Variants: []eval.VariantRow{{Name: "stub", RefusalFlags: count}}}}
		out, err := captureStdout(t, func() error { printEval(newRenderer(false), rec); return nil })
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), "refusal flags") != (count > 0) {
			t.Fatalf("output = %s", out)
		}
	}
}

func TestShowMasksSecretLookingRefusalSummaries(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := runlog.Create("api", agent.Task{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{Status: agent.StatusOK, RefusalFlags: []agent.RefusalFlag{{Kind: "script", Rule: "permission", Refused: "API_KEY=fake-refusal-secret", Later: "bash send.sh"}}}); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return cmdShow([]string{w.ID()}) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "fake-refusal-secret") || !strings.Contains(string(out), "refusal flag (script)") || !strings.Contains(string(out), "••••") {
		t.Fatalf("output = %s", out)
	}
}
