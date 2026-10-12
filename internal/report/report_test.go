package report

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/runlog"
)

var t0 = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

func rec(backend, model, dir string, started time.Duration, res agent.Result) runlog.Record {
	res.Backend = backend
	res.Model = model
	return runlog.Record{ID: backend + started.String(), Backend: backend, Started: t0.Add(started),
		Task: agent.Task{Prompt: "p", Dir: dir}, Result: res, Done: true}
}

// Four runs: two api (estimated cost, cached tokens), two claude (reported
// cost, no token counts), one of them failed.
func sample() []runlog.Record {
	return []runlog.Record{
		rec("api", "claude-opus-5", "/repo/a", 0, agent.Result{Status: agent.StatusOK, Duration: 10 * time.Second, Turns: 5,
			CostUSD: 0.10, CostEstimate: true, InputTokens: 100, CacheRead: 300}),
		rec("api", "claude-opus-5", "/repo/a", time.Hour, agent.Result{Status: agent.StatusOK, Duration: 20 * time.Second, Turns: 7,
			CostUSD: 0.30, CostEstimate: true, InputTokens: 100, CacheRead: 500}),
		rec("claude", "claude-opus-5[1m]", "/repo/b", 2*time.Hour, agent.Result{Status: agent.StatusOK, Duration: 60 * time.Second, Turns: 9, CostUSD: 0.50}),
		rec("claude", "claude-opus-5[1m]", "/repo/a", 3*time.Hour, agent.Result{Status: agent.StatusError, Duration: 30 * time.Second, Turns: 3, CostUSD: 0.20}),
	}
}

func sampleBatch() batch.Result {
	att := func(cost float64, est bool) batch.Attempt {
		return batch.Attempt{Backend: "api", Result: agent.Result{Status: agent.StatusOK, CostUSD: cost, CostEstimate: est}}
	}
	return batch.Result{ID: "b1", Started: t0.Add(time.Hour), Dir: "/repo/a", Tasks: []batch.TaskResult{
		{ID: "t1", Status: batch.StatusPass, Attempts: []batch.Attempt{att(0.30, true)}},
		{ID: "t2", Status: batch.StatusPass, Attempts: []batch.Attempt{att(0.20, false), att(0.25, false)}},
		{ID: "t3", Status: batch.StatusFail, Attempts: []batch.Attempt{att(0.10, false)}},
	}}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestRunRowsByBackend(t *testing.T) {
	d := Select(sample(), nil, time.Time{})
	rows, all, err := d.RunRows(ByBackend)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Key != "api" || rows[1].Key != "claude" {
		t.Fatalf("rows = %+v", rows)
	}
	api := rows[0]
	if api.Runs != 2 || api.OK != 2 || api.MedianDuration != 15*time.Second || !near(api.MedianTurns, 6) ||
		!near(api.TotalCost, 0.40) || !near(api.MedianCost, 0.20) || !api.CostEstimated || !near(api.CacheShare, 0.8) {
		t.Errorf("api row = %+v", api)
	}
	claude := rows[1]
	if claude.Runs != 2 || claude.OK != 1 || claude.MedianDuration != 45*time.Second || !near(claude.MedianTurns, 6) ||
		!near(claude.TotalCost, 0.70) || !near(claude.MedianCost, 0.35) || claude.CostEstimated || claude.CacheShare != -1 {
		t.Errorf("claude row = %+v", claude)
	}
	if all.Key != "all" || all.Runs != 4 || all.OK != 3 || all.MedianDuration != 25*time.Second || !near(all.MedianTurns, 6) ||
		!near(all.TotalCost, 1.10) || !near(all.MedianCost, 0.25) || !all.CostEstimated || !near(all.CacheShare, 0.8) {
		t.Errorf("all row = %+v", all)
	}
}

func TestRunRowsByModelAndDir(t *testing.T) {
	d := Select(sample(), nil, time.Time{})
	rows, _, err := d.RunRows(ByModel)
	if err != nil || len(rows) != 2 || rows[0].Key != "claude-opus-5" || rows[1].Key != "claude-opus-5[1m]" {
		t.Errorf("by model: rows = %+v err=%v", rows, err)
	}
	rows, _, err = d.RunRows(ByDir)
	if err != nil || len(rows) != 2 || rows[0].Key != filepath.Clean("/repo/a") || rows[0].Runs != 3 || rows[1].Key != filepath.Clean("/repo/b") || rows[1].Runs != 1 {
		t.Errorf("by dir: rows = %+v err=%v", rows, err)
	}
	if _, _, err := d.RunRows("colour"); err == nil || !strings.Contains(err.Error(), "backend, model, dir or purpose") {
		t.Errorf("unknown key should be refused: %v", err)
	}
}

func TestRunRowsByPurposeGroupOnTheCategory(t *testing.T) {
	recs := sample()
	recs[0].Task.Prompt = "Fix the hang in batch."
	recs[1].Task.Prompt = "Implement the purpose classifier.\n\nThen review the diff."
	recs[2].Task.Prompt = "Add a --by purpose flag."
	recs[3].Task.Prompt = "Hello there."
	rows, _, err := Select(recs, nil, time.Time{}).RunRows(ByPurpose)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].Key != "implement" || rows[0].Runs != 2 ||
		rows[1].Key != "debug" || rows[1].Runs != 1 || rows[2].Key != "unknown" || rows[2].Runs != 1 {
		t.Errorf("rows = %+v", rows)
	}
}

func TestModelFallsBackToTheTask(t *testing.T) {
	r := rec("api", "", "/repo", 0, agent.Result{Status: agent.StatusOK})
	r.Task.Model = "asked-for"
	rows, _, _ := Select([]runlog.Record{r}, nil, time.Time{}).RunRows(ByModel)
	if rows[0].Key != "asked-for" {
		t.Errorf("key = %q", rows[0].Key)
	}
	r.Task.Model = ""
	rows, _, _ = Select([]runlog.Record{r}, nil, time.Time{}).RunRows(ByModel)
	if rows[0].Key != "(unknown)" {
		t.Errorf("key = %q", rows[0].Key)
	}
}

func TestCacheShareCountsCacheWritesAsInput(t *testing.T) {
	r := rec("api", "m", "/repo", 0, agent.Result{Status: agent.StatusOK, InputTokens: 100, CacheWrite: 300, CacheRead: 600})
	rows, _, _ := Select([]runlog.Record{r}, nil, time.Time{}).RunRows(ByBackend)
	if !near(rows[0].CacheShare, 0.6) {
		t.Errorf("cache share = %v, want 0.6", rows[0].CacheShare)
	}
}

func TestDirKeysAreCanonical(t *testing.T) {
	dir := t.TempDir()
	recs := []runlog.Record{
		rec("api", "m", dir, 0, agent.Result{Status: agent.StatusOK}),
		rec("api", "m", dir+string(filepath.Separator)+".", time.Hour, agent.Result{Status: agent.StatusOK}),
		rec("api", "m", "/gone/repo/", 2*time.Hour, agent.Result{}),
		rec("api", "m", "/gone/repo", 3*time.Hour, agent.Result{}),
	}
	rows, _, _ := Select(recs, nil, time.Time{}).RunRows(ByDir)
	if len(rows) != 2 || rows[0].Runs != 2 || rows[1].Runs != 2 {
		t.Errorf("two spellings of one directory must share a row: %+v", rows)
	}
	b, b2 := sampleBatch(), sampleBatch()
	b2.ID, b2.Dir = "b2", b.Dir+"/"
	if brows, _ := Select(nil, []batch.Result{b, b2}, time.Time{}).BatchRows(); len(brows) != 1 || brows[0].Batches != 2 {
		t.Errorf("batch rows = %+v", brows)
	}
}

func TestSinceAndUnfinished(t *testing.T) {
	recs := sample()
	pending := rec("api", "m", "/repo/a", 4*time.Hour, agent.Result{})
	pending.Done = false
	recs = append(recs, pending)
	batches := []batch.Result{sampleBatch(), {ID: "old", Started: t0.Add(-time.Hour), Dir: "/repo/z"},
		{ID: "live", Started: t0.Add(2 * time.Hour), Dir: "/repo/a", Tasks: []batch.TaskResult{
			{ID: "done", Status: batch.StatusPass}, {ID: "going", Status: batch.StatusRunning}}}}

	d := Select(recs, batches, t0.Add(90*time.Minute))
	if len(d.Runs) != 2 || d.Runs[0].Backend != "claude" || d.Unfinished != 1 || len(d.Batches) != 0 || d.RunningBatches != 1 {
		t.Errorf("window: runs=%d unfinished=%d batches=%d running=%d", len(d.Runs), d.Unfinished, len(d.Batches), d.RunningBatches)
	}
	d = Select(recs, batches, time.Time{})
	if len(d.Runs) != 4 || d.Unfinished != 1 || len(d.Batches) != 2 || d.RunningBatches != 1 {
		t.Errorf("no window: runs=%d unfinished=%d batches=%d running=%d", len(d.Runs), d.Unfinished, len(d.Batches), d.RunningBatches)
	}
	if rep, _ := d.Report(""); rep.RunningBatches != 1 || rep.BatchesAll.Tasks != 3 {
		t.Errorf("a running batch must be counted, not tabulated: %+v", rep)
	}
}

func TestBatchRows(t *testing.T) {
	d := Select(nil, []batch.Result{sampleBatch()}, time.Time{})
	rows, all := d.BatchRows()
	if len(rows) != 1 || rows[0].Key != filepath.Clean("/repo/a") {
		t.Fatalf("rows = %+v", rows)
	}
	for _, r := range []BatchRow{rows[0], all} {
		if r.Batches != 1 || r.Tasks != 3 || r.Passed != 2 || !near(r.MedianAttempts, 1) || !near(r.TotalCost, 0.85) || !r.CostEstimated {
			t.Errorf("row = %+v", r)
		}
	}
	if all.Key != "all" {
		t.Errorf("all key = %q", all.Key)
	}
	b := sampleBatch()
	b.Tasks = append(b.Tasks, batch.TaskResult{ID: "t4", Status: batch.StatusCanceled})
	if _, all := Select(nil, []batch.Result{b}, time.Time{}).BatchRows(); all.Tasks != 4 || all.Passed != 2 || !near(all.MedianAttempts, 1) {
		t.Errorf("a task that never ran counts as a task but not in the attempts median: %+v", all)
	}
	if rows, all := Select(nil, nil, time.Time{}).BatchRows(); len(rows) != 0 || all.Tasks != 0 || all.MedianAttempts != 0 {
		t.Errorf("empty: rows=%v all=%+v", rows, all)
	}
}

func TestMedian(t *testing.T) {
	for _, tc := range []struct {
		xs   []float64
		want float64
	}{
		{nil, 0},
		{[]float64{4}, 4},
		{[]float64{9, 1, 5}, 5},
		{[]float64{4, 1, 3, 2}, 2.5},
	} {
		if got := Median(tc.xs); !near(got, tc.want) {
			t.Errorf("Median(%v) = %v, want %v", tc.xs, got, tc.want)
		}
	}
	xs := []float64{3, 1, 2}
	Median(xs)
	if xs[0] != 3 {
		t.Error("Median must not sort its input")
	}
}

func TestParseSince(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"", 0, true}, {"0", 0, true}, {"7d", 7 * 24 * time.Hour, true}, {"1.5d", 36 * time.Hour, true},
		{"36h", 36 * time.Hour, true}, {"90m", 90 * time.Minute, true},
		{"soon", 0, false}, {"-1d", 0, false}, {"-2h", 0, false}, {"d", 0, false},
		{"Infd", 0, false}, {"NaNd", 0, false}, {"1e12d", 0, false}, {"1e5d", 100000 * 24 * time.Hour, true},
	} {
		got, err := ParseSince(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("ParseSince(%q) = %v, %v; want %v, ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

func TestFormatting(t *testing.T) {
	if Percent(5, 6) != "83%" || Percent(0, 0) != "-" || Percent(3, 3) != "100%" || Percent(0, 3) != "0%" {
		t.Error("Percent")
	}
	if Percent(199, 200) != "99%" || Percent(1, 200) != "1%" {
		t.Errorf("Percent must not round to a false all or nothing: %s %s", Percent(199, 200), Percent(1, 200))
	}
	if Money(0, true) != "-" || Money(1.034, false) != "$1.03" || Money(1.034, true) != "~$1.03" || Money(0.0042, false) != "$0.0042" {
		t.Errorf("Money: %q %q %q %q", Money(0, true), Money(1.034, false), Money(1.034, true), Money(0.0042, false))
	}
	if Number(9) != "9" || Number(6.5) != "6.5" {
		t.Error("Number")
	}
	if Share(-1) != "-" || Share(0.8) != "80%" {
		t.Error("Share")
	}
}

func TestReportEnvelope(t *testing.T) {
	rep, err := Select(nil, nil, time.Time{}).Report("")
	if err != nil || rep.By != ByBackend {
		t.Fatalf("empty report: %+v err=%v", rep, err)
	}
	data, _ := json.Marshal(rep)
	for _, want := range []string{`"since":null`, `"by":"backend"`, `"runs":[]`, `"runs_all":{`, `"unfinished":0`, `"batches":[]`, `"batches_all":{`, `"running_batches":0`, `"unreadable":0`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("empty report JSON is missing %s: %s", want, data)
		}
	}

	since := t0.Add(90 * time.Minute)
	rep, err = Select(sample(), []batch.Result{sampleBatch()}, since).Report(ByModel)
	if err != nil || rep.Since == nil || !rep.Since.Equal(since) || rep.By != ByModel || len(rep.Runs) != 1 || rep.RunsAll.Runs != 2 || len(rep.Batches) != 0 {
		t.Errorf("windowed report: %+v err=%v", rep, err)
	}
	if _, err := Select(nil, nil, time.Time{}).Report("colour"); err == nil {
		t.Error("an unknown key must be refused")
	}
}

func TestRowJSON(t *testing.T) {
	data, _ := json.Marshal(Row{Key: "api", Runs: 1, CacheShare: -1})
	for _, key := range []string{`"key":"api"`, `"runs":1`, `"ok":0`, `"median_duration_ns":0`, `"median_turns":0`, `"total_cost_usd":0`, `"median_cost_usd":0`, `"cache_share":-1`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("row JSON is missing %s: %s", key, data)
		}
	}
	if strings.Contains(string(data), "cost_is_estimate") {
		t.Errorf("a reported cost must not carry the estimate flag: %s", data)
	}
	data, _ = json.Marshal(BatchRow{Key: "x", CostEstimated: true})
	for _, key := range []string{`"batches":0`, `"tasks":0`, `"passed":0`, `"median_attempts":0`, `"total_cost_usd":0`, `"cost_is_estimate":true`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("batch row JSON is missing %s: %s", key, data)
		}
	}
}

// Collect reads what the CLI, the MCP server and batch write, so the test goes
// through their public writers rather than hand-made files.
func TestCollectReadsHome(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	task := agent.Task{Prompt: "p", Dir: t.TempDir(), Options: agent.Options{Mode: agent.ModeAsk}}
	for i, res := range []agent.Result{
		{Backend: "api", Status: agent.StatusOK, Turns: 2, CostUSD: 0.5, CostEstimate: true},
		{Backend: "claude", Status: agent.StatusError, Turns: 1},
	} {
		w, err := runlog.Create(res.Backend, task)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Close(res); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	// A killed run is a task.json with no result.json; writing it by hand keeps
	// no file open, which Windows would otherwise hold against the temp dir.
	pending := filepath.Join(runlog.Home(), "runs", "20260921-000000-pending")
	if err := os.MkdirAll(pending, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(runlog.Record{ID: "20260921-000000-pending", Backend: "api", Started: time.Now(), Task: task})
	if err := os.WriteFile(filepath.Join(pending, "task.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	bdir := filepath.Join(runlog.Home(), "batches")
	if err := os.MkdirAll(bdir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ = json.Marshal(sampleBatch())
	if err := os.WriteFile(filepath.Join(bdir, "b1.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bdir, "broken.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	brokenRun := filepath.Join(runlog.Root(), "20990101-000000-broken")
	if err := os.MkdirAll(brokenRun, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brokenRun, "result.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}

	d, err := Collect(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Runs) != 2 || d.Unfinished != 1 || len(d.Batches) != 1 || d.Batches[0].ID != "b1" || d.Unreadable != 2 {
		t.Fatalf("runs=%d unfinished=%d batches=%+v unreadable=%d", len(d.Runs), d.Unfinished, d.Batches, d.Unreadable)
	}
	rows, all, _ := d.RunRows(ByBackend)
	if len(rows) != 2 || all.Runs != 2 || all.OK != 1 || !near(all.TotalCost, 0.5) || !all.CostEstimated {
		t.Errorf("rows=%+v all=%+v", rows, all)
	}
	if d, err := Collect(time.Now().Add(time.Hour)); err != nil || len(d.Runs) != 0 || len(d.Batches) != 0 {
		t.Errorf("a window in the future should select nothing: %+v err=%v", d, err)
	}
}

func TestCollectWithoutHome(t *testing.T) {
	t.Setenv("HRN_HOME", filepath.Join(t.TempDir(), "never-written"))
	d, err := Collect(time.Time{})
	if err != nil || len(d.Runs) != 0 || len(d.Batches) != 0 {
		t.Errorf("a missing home is an empty report, not an error: %+v err=%v", d, err)
	}
}

func TestBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 bytes", 512: "512 bytes", 1536: "1.5 KB", 18739: "18.3 KB", 5 << 20: "5.0 MB", 3 << 30: "3.0 GB"} {
		if got := Bytes(n); got != want {
			t.Errorf("Bytes(%d) = %q, want %q", n, got, want)
		}
	}
}
