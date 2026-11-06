// name: report-digest-tests
// summary: Check the digest summary, caps, unknown costs, empty windows and input errors.
// effects: read
// parameters: none
// example: go test -count=1 ./scripts/report-digest
// hash: 9df2751533f46bee4df620417d6653649eabba3cad5f1a86d397c3f8c88c9b79
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/report"
)

func summariseJSON(t *testing.T, reportJSON, shapesJSON string, days int) digest {
	t.Helper()
	var report reportInput
	var shapes shapesInput
	if err := json.Unmarshal([]byte(reportJSON), &report); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(shapesJSON), &shapes); err != nil {
		t.Fatal(err)
	}
	got, err := summarise(report, shapes, days)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestSummaryReportsEveryField(t *testing.T) {
	got := summariseJSON(t, `{
		"since":"window-start","by":"purpose",
		"runs":[{"key":"build","runs":3,"ok":2,"median_cost_usd":1.235,"runs_without_cost":1}],
		"runs_all":{"runs":3,"ok":2,"median_duration_ns":1500000000,"median_turns":8,
			"total_cost_usd":2.345,"median_cost_usd":1.235,"runs_without_cost":1,
			"cost_is_estimate":true,"cache_share":0.123456},
		"unfinished":2,"unreadable":1,
		"batches":[{"key":"repo-a","tasks":3,"passed":1}],
		"batches_all":{"batches":2,"tasks":3,"passed":1,"median_attempts":2,
			"total_cost_usd":3.456,"tasks_without_cost":1,"cost_is_estimate":true},
		"running_batches":1
	}`, `{"shapes":[{"shape":"json","bytes":90,"tool_result_share":0.666666}],
		"tool_result_bytes":135,"repeated_inline_script_bytes":42,"runs":{"read":4}}`, 7)
	want := `{"since":"window-start","days":7,"runs":{"runs":3,"ok":2,"ok_share":0.6667,"median_duration_s":2,"total_cost_usd":2.35,"median_cost_usd":1.24,"runs_without_cost":1,"cost_is_estimate":true,"cache_share":0.1235,"unfinished":2,"unreadable":1},"purposes":[{"purpose":"build","runs":3,"ok_share":0.6667,"median_cost_usd":1.24}],"batches":{"batches":2,"tasks":3,"pass_share":0.3333,"median_attempts":2,"total_cost_usd":3.46,"tasks_without_cost":1,"cost_is_estimate":true,"lowest":[{"group":"repo-a","tasks":3,"pass_share":0.3333}]},"tool_output":{"top_shape":"json","top_share":0.6667,"repeated_inline_script_bytes":42,"runs_read":4}}`
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != want {
		t.Fatalf("summary = %s, want %s", encoded, want)
	}
}

func TestPurposesAreSortedByRunsAndCappedAtTen(t *testing.T) {
	rows := []string{`{"key":"first","runs":9}`}
	for i := 11; i >= 0; i-- {
		rows = append(rows, fmt.Sprintf(`{"key":"purpose-%02d","runs":2}`, i))
	}
	got := summariseJSON(t, `{"runs":[`+strings.Join(rows, ",")+`]}`, `{}`, 7)
	if len(got.Purposes) != 10 || got.Purposes[0].Purpose != "first" {
		t.Fatalf("purposes = %+v", got.Purposes)
	}
	for i, purpose := range got.Purposes[1:] {
		if want := fmt.Sprintf("purpose-%02d", i); purpose.Purpose != want {
			t.Fatalf("purpose %d = %q, want %q", i+1, purpose.Purpose, want)
		}
	}
}

func TestUnknownCostsStayNullWhilePartlyKnownOnesStayNumbers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		missing int
		known   bool
	}{
		{"all unknown", 2, false},
		{"missing exceeds count", 3, false},
		{"partly known", 1, true},
		{"known zero", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := 0.0
			if tc.missing > 0 && tc.known {
				value = 0.5
			}
			row := fmt.Sprintf(`{"runs":2,"runs_without_cost":%d,"total_cost_usd":%v,"median_cost_usd":%v}`, tc.missing, value, value)
			report := fmt.Sprintf(`{"runs_all":%s,"runs":[%s],"batches_all":{"tasks":2,"tasks_without_cost":%d,"total_cost_usd":%v}}`, row, row, tc.missing, value)
			got := summariseJSON(t, report, `{}`, 7)
			for _, cost := range []*float64{got.Runs.TotalCostUSD, got.Runs.MedianCostUSD, got.Purposes[0].MedianCostUSD, got.Batches.TotalCostUSD} {
				if (cost != nil) != tc.known {
					t.Fatalf("cost = %v, known = %v", cost, tc.known)
				}
				if cost != nil && *cost != value {
					t.Fatalf("known cost = %v, want %v", *cost, value)
				}
			}
		})
	}
}

func TestEmptyWindowGivesNullsAndEmptyLists(t *testing.T) {
	// hrn reports an unknown cache share as -1.
	got := summariseJSON(t, `{"runs_all":{"key":"all","cache_share":-1}}`, `{}`, 7)
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"since":"","days":7,"runs":{"runs":0,"ok":0,"ok_share":null,"median_duration_s":0,"total_cost_usd":null,"median_cost_usd":null,"runs_without_cost":0,"cost_is_estimate":false,"cache_share":null,"unfinished":0,"unreadable":0},"purposes":[],"batches":{"batches":0,"tasks":0,"pass_share":null,"median_attempts":0,"total_cost_usd":null,"tasks_without_cost":0,"cost_is_estimate":false,"lowest":[]},"tool_output":{"top_shape":null,"top_share":null,"repeated_inline_script_bytes":0,"runs_read":0}}`
	if string(encoded) != want {
		t.Fatalf("summary = %s, want %s", encoded, want)
	}
	zero := summariseJSON(t, `{"runs":[{"key":"empty"}]}`, `{}`, 7)
	if zero.Purposes[0].OKShare != nil || zero.Purposes[0].MedianCostUSD != nil {
		t.Fatalf("empty purpose = %+v", zero.Purposes[0])
	}
}

func TestDigestReadsTheJSONThatHrnReportWrites(t *testing.T) {
	data, err := json.Marshal(report.Report{
		By:         "purpose",
		Runs:       []report.Row{{Key: "review", Runs: 2, OK: 2, CacheShare: -1}},
		RunsAll:    report.Row{Key: "all", Runs: 2, OK: 2, CacheShare: -1},
		BatchesAll: report.BatchRow{Key: "all", Batches: 1, Tasks: 2, Passed: 2, MedianAttempts: 1.5},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := summariseJSON(t, string(data), `{}`, 7)
	if got.Runs.CacheShare != nil || got.Batches.MedianAttempts != 1.5 || got.Purposes[0].Purpose != "review" {
		t.Fatalf("digest = %+v", got)
	}
}

func TestFractionalMedianAttemptsAreKept(t *testing.T) {
	got := summariseJSON(t, `{"batches_all":{"batches":1,"tasks":2,"passed":2,"median_attempts":1.5}}`, `{}`, 7)
	if got.Batches.MedianAttempts != 1.5 {
		t.Fatalf("median attempts = %v, want 1.5", got.Batches.MedianAttempts)
	}
}

func TestBatchCostIsNullWhenTheOnlyStartedTaskHasNoCost(t *testing.T) {
	// tasks counts unstarted tasks too; tasks_without_cost counts started ones only.
	got := summariseJSON(t, `{"batches_all":{"batches":1,"tasks":3,"passed":0,"tasks_without_cost":1,"total_cost_usd":0}}`, `{}`, 7)
	if got.Batches.TotalCostUSD != nil {
		t.Fatalf("total cost = %v, want null", *got.Batches.TotalCostUSD)
	}
}

func TestRunCostsKnownToBeZeroStayZeroBesideUnknownOnes(t *testing.T) {
	// runs_without_cost counts every run, so a zero beside it is a known zero.
	row := `{"key":"local","runs":2,"runs_without_cost":1,"total_cost_usd":0,"median_cost_usd":0}`
	got := summariseJSON(t, `{"runs_all":`+row+`,"runs":[`+row+`]}`, `{}`, 7)
	for _, cost := range []*float64{got.Runs.TotalCostUSD, got.Runs.MedianCostUSD, got.Purposes[0].MedianCostUSD} {
		if cost == nil || *cost != 0 {
			t.Fatalf("run cost = %v, want 0", cost)
		}
	}
}

func TestLowestGroupsAreSortedAndCappedAtThree(t *testing.T) {
	got := summariseJSON(t, `{"batches":[
		{"key":"perfect","tasks":9,"passed":9},
		{"key":"empty"},
		{"key":"small","tasks":2,"passed":1},
		{"key":"repo-b","tasks":4,"passed":2},
		{"key":"repo-a","tasks":4,"passed":2},
		{"key":"worst","tasks":3,"passed":0}
	]}`, `{}`, 7)
	want := []lowestGroup{{Group: "worst", Tasks: 3, PassShare: 0}, {Group: "repo-a", Tasks: 4, PassShare: 0.5}, {Group: "repo-b", Tasks: 4, PassShare: 0.5}}
	if !reflect.DeepEqual(got.Batches.Lowest, want) {
		t.Fatalf("lowest = %+v, want %+v", got.Batches.Lowest, want)
	}
}

func TestTopShapeIsTheLargestWithTiesByName(t *testing.T) {
	for _, tc := range []struct {
		name   string
		shapes string
		want   string
	}{
		{"largest", `{"tool_result_bytes":100,"shapes":[{"shape":"a","bytes":20},{"shape":"b","bytes":80,"tool_result_share":0.812345}]}`, "b"},
		{"tie", `{"tool_result_bytes":100,"shapes":[{"shape":"b","bytes":50},{"shape":"a","bytes":50,"tool_result_share":0.812345}]}`, "a"},
		{"zero bytes", `{"shapes":[{"shape":"a","bytes":50}]}`, ""},
		{"no shapes", `{"tool_result_bytes":100}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := summariseJSON(t, `{}`, tc.shapes, 7).ToolOutput
			if tc.want == "" {
				if got.TopShape != nil || got.TopShare != nil {
					t.Fatalf("top = %+v", got)
				}
			} else if got.TopShape == nil || *got.TopShape != tc.want || got.TopShare == nil || *got.TopShare != 0.8123 {
				t.Fatalf("top = %+v", got)
			}
		})
	}
}

func TestRunPrintsOneLineOrAnErrorObject(t *testing.T) {
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "report.json")
	shapesPath := filepath.Join(dir, "shapes.json")
	badPath := filepath.Join(dir, "bad.json")
	for path, contents := range map[string]string{reportPath: `{}`, shapesPath: `{}`, badPath: `{"secret-content":`} {
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"success", []string{"--report", reportPath, "--shapes", shapesPath}, 0},
		{"bad report", []string{"--report", badPath, "--shapes", shapesPath}, 1},
		{"bad shapes", []string{"--report", reportPath, "--shapes", badPath}, 1},
		{"missing report", []string{"--report", filepath.Join(dir, "missing"), "--shapes", shapesPath}, 1},
		{"missing shapes", []string{"--report", reportPath, "--shapes", filepath.Join(dir, "missing")}, 1},
		{"missing flags", nil, 1},
		{"zero days", []string{"--report", reportPath, "--shapes", shapesPath, "--days", "0"}, 1},
		{"negative days", []string{"--report", reportPath, "--shapes", shapesPath, "--days", "-1"}, 1},
		{"unknown flag", []string{"--unknown"}, 1},
		{"positional argument", []string{"unexpected"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tc.args, &stdout, &stderr); code != tc.code {
				t.Fatalf("exit = %d, want %d", code, tc.code)
			}
			output, unused := stdout.String(), stderr.String()
			if tc.code != 0 {
				output, unused = unused, output
			}
			if unused != "" || strings.Count(output, "\n") != 1 || !strings.HasSuffix(output, "\n") || !json.Valid([]byte(output)) {
				t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
			}
			if tc.code != 0 {
				var got map[string]string
				if err := json.Unmarshal([]byte(output), &got); err != nil || len(got) != 1 || got["error"] == "" || strings.Contains(output, "secret-content") {
					t.Fatalf("error output = %q", output)
				}
			}
		})
	}
}

func TestSummariseRejectsNonPositiveDays(t *testing.T) {
	for _, days := range []int{0, -1} {
		if _, err := summarise(reportInput{}, shapesInput{}, days); err == nil {
			t.Fatalf("days %d accepted", days)
		}
	}
}
