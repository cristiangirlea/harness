package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/eval"
	"github.com/cristiangirlea/harness/internal/kernel"
	"github.com/cristiangirlea/harness/internal/report"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/ws"
)

func TestJSONOutputs(t *testing.T) {
	rec := runlog.Record{RecordVersion: 1, ID: "run", Backend: "codex"}
	events := []kernel.Event{{Kind: kernel.EventText, Text: "done"}}
	turns := []runlog.Turn{{N: 1}}
	b := batch.Result{ID: "batch", Tasks: []batch.TaskResult{{ID: "task", Status: batch.StatusPass}}}
	d := eval.Diff{A: "before", B: "after"}
	r := report.Report{By: "backend", Unfinished: 2}
	slots := []ws.Status{{N: 2, State: "free"}}
	issues := []ws.IssueStatus{{Issue: "org/repo#3", State: "held"}}
	c := eval.RetrievalComparison{EvidenceKind: "fixture", ExpectedPairs: 2}

	testJSONOutput(t, "runs", runsDocument{Runs: []runJSON{{ID: "run", Backend: "codex", Status: "running"}}},
		func(w io.Writer) error { return printRunsJSON(w, []runlog.Record{rec}) })
	testJSONOutput(t, "show", showJSON{Record: rec, Events: events},
		func(w io.Writer) error { return printShowJSON(w, rec, events) })
	testJSONOutput(t, "show_usage", usageJSON{Turns: turns},
		func(w io.Writer) error { return printUsageJSON(w, turns) })
	testJSONOutput(t, "batch", batchJSON{Result: &b},
		func(w io.Writer) error { return printBatchJSON(w, &b) })
	testJSONOutput(t, "eval_diff", evalDiffJSON{Diff: &d},
		func(w io.Writer) error { return printEvalDiffJSON(w, &d) })
	testJSONOutput(t, "report", reportJSON{Report: &r},
		func(w io.Writer) error { return printReportJSON(w, &r) })
	testJSONOutput(t, "ws_list", workspacesJSON{Workspaces: slots},
		func(w io.Writer) error { return printWorkspacesJSON(w, slots) })
	testJSONOutput(t, "ws_issues", issuesJSON{Issues: issues},
		func(w io.Writer) error { return printIssuesJSON(w, issues) })
	testJSONOutput(t, "retrieval", retrievalJSON{RetrievalComparison: &c},
		func(w io.Writer) error { return printRetrievalJSON(w, &c) })
}

func testJSONOutput[T any](t *testing.T, name string, unversioned T, emit func(io.Writer) error) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		var out bytes.Buffer
		if err := emit(&out); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join("testdata", "golden", name+".json")
		if os.Getenv("UPDATE_GOLDEN") == "1" {
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, out.Bytes(), 0644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out.Bytes(), want) {
			t.Errorf("JSON differs from %s\ngot: %s\nwant: %s", path, out.Bytes(), want)
		}

		t.Run("reader", func(t *testing.T) {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(out.Bytes(), &fields); err != nil {
				t.Fatal(err)
			}
			if string(fields["version"]) != "1" {
				t.Fatalf("version = %s, want 1", fields["version"])
			}
			delete(fields, "version")
			fields["future_field"] = json.RawMessage(`{"extra":true}`)
			data, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			var got T
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, unversioned) {
				t.Errorf("decoded %#v, want %#v", got, unversioned)
			}
		})
	})
}
