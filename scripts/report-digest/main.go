// name: report-digest-main
// summary: Summarise a window of hrn runs: pass rate, cost, batch attempts and the largest tool output shape.
// effects: read
// parameters: --report:path=required; --shapes:path=required; --days:int=7
// example: go test -count=1 ./scripts/report-digest
// hash: 1bb965dfaa807991be7fecb7777a88a9e3ec55ca9fffc399b1b7f998d290a1ef

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"math"
	"os"
	"sort"
)

type runRow struct {
	Key              string  `json:"key"`
	Runs             int     `json:"runs"`
	OK               int     `json:"ok"`
	MedianDurationNS int64   `json:"median_duration_ns"`
	TotalCostUSD     float64 `json:"total_cost_usd"`
	MedianCostUSD    float64 `json:"median_cost_usd"`
	RunsWithoutCost  int     `json:"runs_without_cost"`
	CostIsEstimate   bool    `json:"cost_is_estimate"`
	CacheShare       float64 `json:"cache_share"`
}

type batchRow struct {
	Key              string  `json:"key"`
	Batches          int     `json:"batches"`
	Tasks            int     `json:"tasks"`
	Passed           int     `json:"passed"`
	MedianAttempts   float64 `json:"median_attempts"`
	TotalCostUSD     float64 `json:"total_cost_usd"`
	CostIsEstimate   bool    `json:"cost_is_estimate"`
	TasksWithoutCost int     `json:"tasks_without_cost"`
}

type reportInput struct {
	Since      string     `json:"since"`
	Runs       []runRow   `json:"runs"`
	RunsAll    runRow     `json:"runs_all"`
	Unfinished int        `json:"unfinished"`
	Batches    []batchRow `json:"batches"`
	BatchesAll batchRow   `json:"batches_all"`
	Unreadable int        `json:"unreadable"`
}

type shapeRow struct {
	Shape           string  `json:"shape"`
	Bytes           int64   `json:"bytes"`
	ToolResultShare float64 `json:"tool_result_share"`
}

type shapesInput struct {
	Shapes                    []shapeRow `json:"shapes"`
	ToolResultBytes           int64      `json:"tool_result_bytes"`
	RepeatedInlineScriptBytes int64      `json:"repeated_inline_script_bytes"`
	Runs                      struct {
		Read int `json:"read"`
	} `json:"runs"`
}

type runSummary struct {
	Runs            int      `json:"runs"`
	OK              int      `json:"ok"`
	OKShare         *float64 `json:"ok_share"`
	MedianDurationS int64    `json:"median_duration_s"`
	TotalCostUSD    *float64 `json:"total_cost_usd"`
	MedianCostUSD   *float64 `json:"median_cost_usd"`
	RunsWithoutCost int      `json:"runs_without_cost"`
	CostIsEstimate  bool     `json:"cost_is_estimate"`
	CacheShare      *float64 `json:"cache_share"`
	Unfinished      int      `json:"unfinished"`
	Unreadable      int      `json:"unreadable"`
}

type purposeSummary struct {
	Purpose       string   `json:"purpose"`
	Runs          int      `json:"runs"`
	OKShare       *float64 `json:"ok_share"`
	MedianCostUSD *float64 `json:"median_cost_usd"`
}

type lowestGroup struct {
	Group     string  `json:"group"`
	Tasks     int     `json:"tasks"`
	PassShare float64 `json:"pass_share"`
}

type batchSummary struct {
	Batches          int           `json:"batches"`
	Tasks            int           `json:"tasks"`
	PassShare        *float64      `json:"pass_share"`
	MedianAttempts   float64       `json:"median_attempts"`
	TotalCostUSD     *float64      `json:"total_cost_usd"`
	TasksWithoutCost int           `json:"tasks_without_cost"`
	CostIsEstimate   bool          `json:"cost_is_estimate"`
	Lowest           []lowestGroup `json:"lowest"`
}

type toolSummary struct {
	TopShape                  *string  `json:"top_shape"`
	TopShare                  *float64 `json:"top_share"`
	RepeatedInlineScriptBytes int64    `json:"repeated_inline_script_bytes"`
	RunsRead                  int      `json:"runs_read"`
}

type digest struct {
	Since      string           `json:"since"`
	Days       int              `json:"days"`
	Runs       runSummary       `json:"runs"`
	Purposes   []purposeSummary `json:"purposes"`
	Batches    batchSummary     `json:"batches"`
	ToolOutput toolSummary      `json:"tool_output"`
}

func rounded(value float64, scale float64) float64 {
	return math.Round(value*scale) / scale
}

func share(numerator, denominator int) *float64 {
	if denominator == 0 {
		return nil
	}
	value := rounded(float64(numerator)/float64(denominator), 10000)
	return &value
}

func cost(value float64, count, missing int) *float64 {
	if count == 0 || missing >= count {
		return nil
	}
	value = rounded(value, 100)
	return &value
}

// Batch tasks count unstarted ones while missing counts only started ones, so a zero beside missing is unknown.
func batchCost(value float64, tasks, missing int) *float64 {
	if value == 0 && missing > 0 {
		return nil
	}
	return cost(value, tasks, missing)
}

// hrn reports an unknown cache share as -1.
func cacheShare(value float64) *float64 {
	if value < 0 {
		return nil
	}
	value = rounded(value, 10000)
	return &value
}

func summarise(report reportInput, shapes shapesInput, days int) (digest, error) {
	if days <= 0 {
		return digest{}, errors.New("days must be positive")
	}
	r := report.RunsAll
	b := report.BatchesAll
	result := digest{
		Since: report.Since,
		Days:  days,
		Runs: runSummary{
			Runs:            r.Runs,
			OK:              r.OK,
			OKShare:         share(r.OK, r.Runs),
			MedianDurationS: int64(math.Round(float64(r.MedianDurationNS) / 1e9)),
			TotalCostUSD:    cost(r.TotalCostUSD, r.Runs, r.RunsWithoutCost),
			MedianCostUSD:   cost(r.MedianCostUSD, r.Runs, r.RunsWithoutCost),
			RunsWithoutCost: r.RunsWithoutCost,
			CostIsEstimate:  r.CostIsEstimate,
			CacheShare:      cacheShare(r.CacheShare),
			Unfinished:      report.Unfinished,
			Unreadable:      report.Unreadable,
		},
		Purposes: []purposeSummary{},
		Batches: batchSummary{
			Batches:          b.Batches,
			Tasks:            b.Tasks,
			PassShare:        share(b.Passed, b.Tasks),
			MedianAttempts:   rounded(b.MedianAttempts, 100),
			TotalCostUSD:     batchCost(b.TotalCostUSD, b.Tasks, b.TasksWithoutCost),
			TasksWithoutCost: b.TasksWithoutCost,
			CostIsEstimate:   b.CostIsEstimate,
			Lowest:           []lowestGroup{},
		},
		ToolOutput: toolSummary{
			RepeatedInlineScriptBytes: shapes.RepeatedInlineScriptBytes,
			RunsRead:                  shapes.Runs.Read,
		},
	}
	rows := append([]runRow(nil), report.Runs...)
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Runs != rows[j].Runs {
			return rows[i].Runs > rows[j].Runs
		}
		return rows[i].Key < rows[j].Key
	})
	for _, row := range rows[:min(10, len(rows))] {
		result.Purposes = append(result.Purposes, purposeSummary{
			Purpose:       row.Key,
			Runs:          row.Runs,
			OKShare:       share(row.OK, row.Runs),
			MedianCostUSD: cost(row.MedianCostUSD, row.Runs, row.RunsWithoutCost),
		})
	}
	var lowest []batchRow
	for _, row := range report.Batches {
		if row.Tasks > 0 && row.Passed < row.Tasks {
			lowest = append(lowest, row)
		}
	}
	sort.Slice(lowest, func(i, j int) bool {
		a, b := lowest[i], lowest[j]
		x, y := float64(a.Passed)/float64(a.Tasks), float64(b.Passed)/float64(b.Tasks)
		if x != y {
			return x < y
		}
		if a.Tasks != b.Tasks {
			return a.Tasks > b.Tasks
		}
		return a.Key < b.Key
	})
	for _, row := range lowest[:min(3, len(lowest))] {
		result.Batches.Lowest = append(result.Batches.Lowest, lowestGroup{
			Group:     row.Key,
			Tasks:     row.Tasks,
			PassShare: *share(row.Passed, row.Tasks),
		})
	}
	if shapes.ToolResultBytes != 0 && len(shapes.Shapes) > 0 {
		top := shapes.Shapes[0]
		for _, row := range shapes.Shapes[1:] {
			if row.Bytes > top.Bytes || (row.Bytes == top.Bytes && row.Shape < top.Shape) {
				top = row
			}
		}
		value := rounded(top.ToolResultShare, 10000)
		result.ToolOutput.TopShape = &top.Shape
		result.ToolOutput.TopShare = &value
	}
	return result, nil
}

func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return errors.New("cannot read input file")
	}
	if err := json.Unmarshal(data, value); err != nil {
		return errors.New("invalid input JSON")
	}
	return nil
}

func run(args []string, stdout, stderr io.Writer) int {
	fail := func(message string) int {
		_ = json.NewEncoder(stderr).Encode(struct {
			Error string `json:"error"`
		}{message})
		return 1
	}
	flags := flag.NewFlagSet("report-digest", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	reportPath := flags.String("report", "", "report JSON file")
	shapesPath := flags.String("shapes", "", "tool shapes JSON file")
	days := flags.Int("days", 7, "window in days")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return fail("invalid arguments")
	}
	if *days <= 0 {
		return fail("days must be positive")
	}
	if *reportPath == "" || *shapesPath == "" {
		return fail("report and shapes are required")
	}
	var report reportInput
	if err := readJSON(*reportPath, &report); err != nil {
		return fail("report: " + err.Error())
	}
	var shapes shapesInput
	if err := readJSON(*shapesPath, &shapes); err != nil {
		return fail("shapes: " + err.Error())
	}
	result, err := summarise(report, shapes, *days)
	if err != nil {
		return fail(err.Error())
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return fail("cannot write digest")
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
