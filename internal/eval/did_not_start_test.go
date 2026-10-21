package eval

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
)

func TestDidNotStartCellsAreCountedApart(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, status := range []agent.Status{agent.StatusError, agent.StatusOK} {
		t.Run(string(status), func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			spec := Spec{Spec: batch.Spec{Tasks: []batch.Task{{ID: "task"}}}, Matrix: []Variant{{Backend: "stub"}}}
			var res batch.Result
			if err := json.Unmarshal([]byte(`{"tasks":[
				{"id":"e.task.stub.r1","status":"pass","commit":"abc","duration_ns":3000000000,"attempts":[{"result":{"status":"ok","turns":2}}]},
				{"id":"e.task.stub.r2","status":"fail","duration_ns":1000000000,"attempts":[{"result":{"status":"error","error":"task failed","turns":1}}]},
				{"id":"e.task.stub.r3","status":"fail","attempts":[{"did_not_start":"launch refused","result":{"status":"error","error":"launch refused","turns":0}}]},
				{"id":"e.task.stub.r4","status":"fail","attempts":[{"result":{"status":"ok","turns":1}}]},
				{"id":"e.task.stub.r5","status":"fail","duration_ns":5000000000,"attempts":[{"result":{"status":"error","error":"old launch refused","turns":0}}]}
			]}`), &res); err != nil {
				t.Fatal(err)
			}
			res.Tasks[2].Attempts[0].Result.Status = status
			if status == agent.StatusOK {
				res.Tasks[2].Attempts[0].Gates = []batch.GateResult{{ExitCode: -1, InfrastructureError: "shell unavailable"}}
			}
			rec := Record{ID: "new", Spec: spec, Table: Summarize(spec, res, nil)}
			v := rec.Table.Variants[0]
			if v.Attempts != 5 || v.Passed != 1 || v.DidNotStart != 1 || v.DidNotAct != 1 || v.PassRate() != 1.0/3 {
				t.Errorf("variant = %+v, pass rate %v", v, v.PassRate())
			}
			if v.Statuses["ok"]+v.Statuses["error"] != 5 || v.Statuses[string(status)] != map[agent.Status]int{agent.StatusError: 3, agent.StatusOK: 3}[status] {
				t.Errorf("statuses = %v", v.Statuses)
			}
			if v.MedianDuration != 3*time.Second || v.MedianTurns != 1 {
				t.Errorf("medians = duration %v, turns %v", v.MedianDuration, v.MedianTurns)
			}
			row := rec.Table.Tasks[0]
			if row.Total != 5 || row.DidNotStart != 1 || row.DidNotAct != 1 || row.Score() != "1/3 +1 did not act +1 did not start" {
				t.Errorf("task = %+v, score %q", row, row.Score())
			}
			if cells, passed, _, _, _, _ := rec.Totals(); cells != 5 || passed != 1 {
				t.Errorf("totals = %d cells, %d passed", cells, passed)
			}
			data, err := json.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			var loaded Record
			if err := json.Unmarshal(data, &loaded); err != nil {
				t.Fatal(err)
			}
			d := Compare(Record{ID: "old"}, loaded)
			if d.Variants[0].B.DidNotStart != 1 || d.Tasks[0].B.DidNotStart != 1 {
				t.Fatalf("diff lost counts: %+v", d)
			}
			data, err = json.Marshal(d)
			if err != nil || strings.Count(string(data), `"did_not_start":1`) != 2 {
				t.Errorf("diff JSON = %s, error %v", data, err)
			}
		})
	}
}

func TestOldEvalRowsKeepTheirDenominators(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	var rec Record
	if err := json.Unmarshal([]byte(`{"table":{"variants":[{"name":"stub","attempts":3,"passed":1,"did_not_act":1}],"tasks":[{"task":"task","variant":"stub","total":3,"passed":1,"did_not_act":1}]}}`), &rec); err != nil {
		t.Fatal(err)
	}
	if v := rec.Table.Variants[0]; v.DidNotStart != 0 || v.PassRate() != .5 {
		t.Errorf("old variant = %+v", v)
	}
	if row := rec.Table.Tasks[0]; row.DidNotStart != 0 || row.Score() != "1/2 +1 did not act" {
		t.Errorf("old task = %+v", row)
	}
	data, err := json.Marshal(rec)
	if err != nil || strings.Contains(string(data), "did_not_start") {
		t.Errorf("old record JSON = %s, error %v", data, err)
	}
	if rate := (VariantRow{Attempts: 1, DidNotStart: 1}).PassRate(); rate != 0 {
		t.Errorf("unstarted-only pass rate = %v", rate)
	}
}
