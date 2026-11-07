package eval

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/kernel"
)

func TestVariantToolStatsTotalAllAttemptsWithStats(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	spec := Spec{Spec: batch.Spec{Tasks: []batch.Task{{ID: "task"}}}, Matrix: []Variant{{Name: "v"}}}
	res := batch.Result{Tasks: []batch.TaskResult{{ID: "e.task.v.r1", Attempts: []batch.Attempt{
		{Result: agent.Result{ToolStats: map[string]kernel.ToolStat{"read_file": {Calls: 2, ResultBytes: 8}}}},
		{Result: agent.Result{ToolStats: map[string]kernel.ToolStat{"read_file": {Calls: 1, Errors: 1, ResultBytes: 3}, "edit_file": {Calls: 1}}}},
		{Result: agent.Result{}},
		{Result: agent.Result{ToolStats: map[string]kernel.ToolStat{"read_file": {Calls: -1, Errors: -1, ResultBytes: -1}}}},
	}}}}
	table := Summarize(spec, res, nil)
	v := table.Variants[0]
	if v.AttemptsWithToolStats != 2 || v.ToolStats["read_file"] != (kernel.ToolStat{Calls: 3, Errors: 1, ResultBytes: 11}) || v.ToolStats["edit_file"].Calls != 1 {
		t.Fatalf("variant = %+v", v)
	}
	data, err := json.Marshal(table)
	if err != nil || !strings.Contains(string(data), `"attempts_with_tool_stats":2`) || !strings.Contains(string(data), `"result_bytes":11`) {
		t.Fatalf("table = %s, error = %v", data, err)
	}
}
