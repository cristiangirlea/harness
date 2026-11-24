package main

import (
	"encoding/json"
	"io"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/eval"
	"github.com/cristiangirlea/harness/internal/kernel"
	"github.com/cristiangirlea/harness/internal/report"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/ws"
)

const (
	runsVersion       = 1
	showVersion       = 1
	usageVersion      = 1
	batchVersion      = 1
	evalDiffVersion   = 1
	reportVersion     = 1
	workspacesVersion = 1
	issuesVersion     = 1
	retrievalVersion  = 1
)

type runsDocument struct {
	Version int       `json:"version"`
	Runs    []runJSON `json:"runs"`
}

type showJSON struct {
	Version int            `json:"version"`
	Record  runlog.Record  `json:"record"`
	Events  []kernel.Event `json:"events"`
}

type usageJSON struct {
	Version int           `json:"version"`
	Turns   []runlog.Turn `json:"turns"`
}

type batchJSON struct {
	Version int `json:"version"`
	*batch.Result
}

type evalDiffJSON struct {
	Version int `json:"version"`
	*eval.Diff
}

type reportJSON struct {
	Version int `json:"version"`
	*report.Report
}

type workspacesJSON struct {
	Version    int         `json:"version"`
	Workspaces []ws.Status `json:"workspaces"`
}

type issuesJSON struct {
	Version int              `json:"version"`
	Issues  []ws.IssueStatus `json:"issues"`
}

type retrievalJSON struct {
	Version int `json:"version"`
	*eval.RetrievalComparison
}

func writeIndentedJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func printShowJSON(w io.Writer, rec runlog.Record, events []kernel.Event) error {
	if events == nil {
		events = []kernel.Event{}
	}
	return writeIndentedJSON(w, showJSON{showVersion, rec, events})
}

func printUsageJSON(w io.Writer, turns []runlog.Turn) error {
	if turns == nil {
		turns = []runlog.Turn{}
	}
	return writeIndentedJSON(w, usageJSON{usageVersion, turns})
}

func printBatchJSON(w io.Writer, result *batch.Result) error {
	return writeIndentedJSON(w, batchJSON{batchVersion, result})
}

func printEvalDiffJSON(w io.Writer, diff *eval.Diff) error {
	return writeIndentedJSON(w, evalDiffJSON{evalDiffVersion, diff})
}

func printReportJSON(w io.Writer, r *report.Report) error {
	return writeIndentedJSON(w, reportJSON{reportVersion, r})
}

func printWorkspacesJSON(w io.Writer, rows []ws.Status) error {
	if rows == nil {
		rows = []ws.Status{}
	}
	return writeIndentedJSON(w, workspacesJSON{workspacesVersion, rows})
}

func printIssuesJSON(w io.Writer, rows []ws.IssueStatus) error {
	if rows == nil {
		rows = []ws.IssueStatus{}
	}
	return writeIndentedJSON(w, issuesJSON{issuesVersion, rows})
}

func printRetrievalJSON(w io.Writer, result *eval.RetrievalComparison) error {
	return writeIndentedJSON(w, retrievalJSON{retrievalVersion, result})
}
