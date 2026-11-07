package report

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/kernel"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestReportTotalsOnlyToolStatsFromSelectedFinishedRuns(t *testing.T) {
	stats := map[string]kernel.ToolStat{"read_file": {Calls: 2, Errors: 1, ResultBytes: 8}}
	now := time.Now()
	d := Select([]runlog.Record{
		{Done: true, Started: now, Result: kernel.Result{ToolStats: stats}},
		{Done: true, Started: now, Result: kernel.Result{ToolStats: stats}},
		{Done: false, Started: now, Result: kernel.Result{ToolStats: stats}},
		{Done: true, Started: now.Add(-2 * time.Hour), Result: kernel.Result{ToolStats: stats}},
		{Done: true, Started: now, Result: kernel.Result{ToolStats: map[string]kernel.ToolStat{"read_file": {Calls: -1, Errors: -1, ResultBytes: -1}}}},
	}, nil, now.Add(-time.Hour))
	rep, err := d.Report(ByBackend)
	if err != nil || rep.ToolStats["read_file"] != (kernel.ToolStat{Calls: 4, Errors: 2, ResultBytes: 16}) {
		t.Fatalf("report = %+v, error = %v", rep, err)
	}
	if stats["read_file"].Calls != 2 {
		t.Fatal("report changed the source stats")
	}
	rep, err = (Data{}).Report(ByBackend)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rep)
	if err != nil || strings.Contains(string(data), "tool_stats") {
		t.Fatalf("report = %s, error = %v", data, err)
	}
}
