package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/report"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestReportCostCells(t *testing.T) {
	for _, tc := range []struct {
		name          string
		results       []agent.Result
		total, median string
	}{
		{"all known", []agent.Result{{CostUSD: 1.23}, {CostUSD: 2.77}}, "$4.00", "$2.00"},
		{"some missing", []agent.Result{{CostUSD: 1.23}, {Usage: &agent.UsageMetadata{CostStatus: "missing"}}, {}}, "$1.23 (2 unknown)", "$1.23"},
		{"none known", []agent.Result{{CostUSD: 9, Usage: &agent.UsageMetadata{CostStatus: "missing"}}, {}}, "-", "-"},
		{"reported zero", []agent.Result{{Usage: &agent.UsageMetadata{CostStatus: "reported"}}}, "$0.00", "$0.00"},
		{"zero and missing", []agent.Result{{Usage: &agent.UsageMetadata{CostStatus: "reported"}}, {}}, "$0.00 (1 unknown)", "$0.00"},
		{"estimated zero", []agent.Result{{Usage: &agent.UsageMetadata{CostStatus: "estimated"}}}, "~$0.00", "~$0.00"},
		{"estimated positive", []agent.Result{{CostUSD: 1.23, Usage: &agent.UsageMetadata{CostStatus: "estimated"}}}, "~$1.23", "~$1.23"},
		{"missing estimate ignored", []agent.Result{{CostUSD: 1.23}, {CostUSD: 9, CostEstimate: true, Usage: &agent.UsageMetadata{CostStatus: "missing"}}}, "$1.23 (1 unknown)", "$1.23"},
		{"known zero median", []agent.Result{{Usage: &agent.UsageMetadata{CostStatus: "reported"}}, {Usage: &agent.UsageMetadata{CostStatus: "reported"}}, {CostUSD: 1.23}}, "$1.23", "$0.00"},
		{"legacy positive", []agent.Result{{CostUSD: 1.23}}, "$1.23", "$1.23"},
		{"sub-cent known", []agent.Result{{CostUSD: 0.0012, Usage: &agent.UsageMetadata{CostStatus: "reported"}}}, "$0.0012", "$0.0012"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			b := batch.Result{ID: "costs", Dir: "repo"}
			for _, res := range tc.results {
				w, err := runlog.Create("codex", agent.Task{Prompt: "p"})
				if err != nil {
					t.Fatal(err)
				}
				res.Status = agent.StatusOK
				if err := w.Close(res); err != nil {
					t.Fatal(err)
				}
				b.Tasks = append(b.Tasks, batch.TaskResult{Status: batch.StatusPass, Attempts: []batch.Attempt{{Result: res}}})
			}
			dir := filepath.Join(runlog.Home(), "batches")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(b)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "costs.json"), data, 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := captureStdout(t, func() error { return cmdReport(nil) })
			if err != nil {
				t.Fatal(err)
			}
			blocks := strings.Split(strings.TrimSpace(string(out)), "\n\n")
			if len(blocks) != 3 {
				t.Fatalf("report=%s", out)
			}
			for _, index := range []int{0, 2} {
				lines := strings.Split(blocks[index], "\n")
				if len(lines) != 4 {
					t.Fatalf("table=%s", blocks[index])
				}
				for _, line := range lines[2:] {
					// Two spaces separate columns; spaces within the unknown count stay intact.
					var columns []string
					for _, c := range strings.Split(line, "  ") {
						if c = strings.TrimSpace(c); c != "" {
							columns = append(columns, c)
						}
					}
					if index == 0 {
						if len(columns) != 9 || columns[6] != tc.total || columns[7] != tc.median {
							t.Errorf("run row=%q, want total %q median %q", line, tc.total, tc.median)
						}
					} else if len(columns) != 6 || columns[5] != tc.total {
						t.Errorf("batch row=%q, want total %q", line, tc.total)
					}
				}
			}
		})
	}
}

func TestReportRoutedTextAndJSON(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := runlog.CreateContinuing("codex", agent.Task{Prompt: "Implement widget"}, "", &runlog.RoutedBy{Category: "implement", Eval: "e1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{Status: agent.StatusOK, CostUSD: 1.5}); err != nil {
		t.Fatal(err)
	}
	for _, jsonOut := range []bool{false, true} {
		f, err := os.CreateTemp(t.TempDir(), "stdout")
		if err != nil {
			t.Fatal(err)
		}
		prev := os.Stdout
		os.Stdout = f
		args := []string{"--by", "routed"}
		if jsonOut {
			args = append(args, "--json")
		}
		err = cmdReport(args)
		os.Stdout = prev
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(f.Name())
		if !strings.Contains(string(data), "implement via e1") {
			t.Fatalf("output: %s", data)
		}
		if jsonOut {
			var rep report.Report
			if err := json.Unmarshal(data, &rep); err != nil || rep.Runs[0].Runs != 1 || rep.Runs[0].OK != 1 || rep.Runs[0].TotalCost != 1.5 {
				t.Fatalf("json: %s %v", data, err)
			}
		} else if !strings.Contains(string(data), "$1.50") {
			t.Fatalf("text: %s", data)
		}
	}
}

func TestReportCostCellsAcrossBackends(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, run := range []struct {
		backend string
		result  agent.Result
	}{
		{"claude", agent.Result{Status: agent.StatusOK, CostUSD: 1.23, Usage: &agent.UsageMetadata{CostStatus: "reported"}}},
		{"codex", agent.Result{Status: agent.StatusOK, Usage: &agent.UsageMetadata{CostStatus: "missing"}}},
	} {
		w, err := runlog.Create(run.backend, agent.Task{Prompt: "p"})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Close(run.result); err != nil {
			t.Fatal(err)
		}
	}
	out, err := captureStdout(t, func() error { return cmdReport(nil) })
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(string(out), "\n\n")
	lines := strings.Split(first, "\n")
	if len(lines) != 5 {
		t.Fatalf("report=%s", out)
	}
	for i, want := range []struct {
		backend, total, median string
	}{
		{"claude", "$1.23", "$1.23"},
		{"codex", "-", "-"},
		{"all", "$1.23 (1 unknown)", "$1.23"},
	} {
		var columns []string
		for _, c := range strings.Split(lines[i+2], "  ") {
			if c = strings.TrimSpace(c); c != "" {
				columns = append(columns, c)
			}
		}
		if len(columns) != 9 || columns[0] != want.backend || columns[6] != want.total || columns[7] != want.median {
			t.Errorf("run row=%q, want %s total %q median %q", lines[i+2], want.backend, want.total, want.median)
		}
	}
}

func TestReportBatchWithoutAttemptsHasZeroCost(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	b := batch.Result{ID: "queued", Dir: "repo", Tasks: []batch.TaskResult{{Status: batch.StatusCanceled}, {Status: batch.StatusCanceled}}}
	dir := filepath.Join(runlog.Home(), "batches")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "queued.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return cmdReport(nil) })
	if err != nil {
		t.Fatal(err)
	}
	_, batches, found := strings.Cut(string(out), "batches by repository, all time\n")
	lines := strings.Split(strings.TrimSpace(batches), "\n")
	if !found || len(lines) != 3 {
		t.Fatalf("report=%s", out)
	}
	for _, line := range lines[1:] {
		columns := strings.Fields(line)
		if len(columns) != 6 || columns[2] != "2" || columns[5] != "$0.00" {
			t.Errorf("batch row=%q, want two tasks with total $0.00", line)
		}
	}
}

func TestTableAlignsColumns(t *testing.T) {
	got := table([][]string{{"key", "n"}, {"a", "10"}, {"longer", "5"}}, 1)
	want := "key      n\na       10\nlonger   5\n"
	if got != want {
		t.Errorf("table =\n%q\nwant\n%q", got, want)
	}
	// A left-aligned last column carries no trailing spaces.
	if got := table([][]string{{"k", "label"}, {"kk", "x"}}, 2); got != "k   label\nkk  x\n" {
		t.Errorf("left-aligned table = %q", got)
	}
}

func TestShortKeyKeepsTheTail(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 4, "abc"},
		{"abcd", 4, "abcd"},
		{"abcdef", 4, "…def"},
		{"héllo wörld", 6, "…wörld"},
	} {
		if got := shortKey(tc.in, tc.n); got != tc.want {
			t.Errorf("shortKey(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestReportGroupsRunsByPurpose(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, prompt := range []string{
		"Fix the hang in batch.",
		"Write unit tests for the parser.\n\nThen fix whatever they find.",
		"Write unit tests for the codec.",
	} {
		w, err := runlog.Create("claude", agent.Task{Prompt: prompt, Dir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Close(agent.Result{Status: agent.StatusOK}); err != nil {
			t.Fatal(err)
		}
	}
	out, err := captureStdout(t, func() error { return cmdReport([]string{"--by", "purpose", "--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var rep report.Report
	if err := json.Unmarshal(out, &rep); err != nil {
		t.Fatalf("%v in %s", err, out)
	}
	if rep.By != "purpose" || len(rep.Runs) != 2 || rep.Runs[0].Key != "test" || rep.Runs[0].Runs != 2 ||
		rep.Runs[1].Key != "debug" || rep.Runs[1].Runs != 1 || rep.RunsAll.Runs != 3 {
		t.Errorf("report = %+v", rep)
	}
	// The table groups the same way: heading, header, a row per purpose, the all row.
	out, err = captureStdout(t, func() error { return cmdReport([]string{"--by", "purpose"}) })
	if err != nil {
		t.Fatal(err)
	}
	// The runs table is the first block; the tokens table follows it.
	first, _, _ := strings.Cut(string(out), "\n\n")
	lines := strings.Split(strings.TrimRight(first, "\n"), "\n")
	if len(lines) != 5 || lines[0] != "runs by purpose, all time" || !strings.HasPrefix(lines[1], "purpose ") {
		t.Fatalf("table =\n%s", out)
	}
	for i, want := range []string{"test 2", "debug 1", "all 3"} {
		f := strings.Fields(lines[2+i])
		if len(f) < 2 || f[0]+" "+f[1] != want {
			t.Errorf("table row %d = %q, want it to start with %q", i, lines[2+i], want)
		}
	}
	if err := cmdReport([]string{"--by", "colour"}); err == nil || !strings.Contains(err.Error(), `unknown key "colour"`) {
		t.Errorf("an unknown --by must be refused: %v", err)
	}
}

// captureStdout runs f with os.Stdout sent to a pipe and returns what it wrote.
func captureStdout(t *testing.T, f func() error) ([]byte, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()
	done := make(chan []byte)
	go func() {
		b, _ := io.ReadAll(r)
		done <- b
	}()
	ferr := f()
	w.Close()
	return <-done, ferr
}

func TestReportByDayPrintsTokensAndContextPerTurn(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := runlog.Create("claude", agent.Task{Prompt: "p", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, ctx := range []int64{10000, 30000, 50000} {
		w.Event(agent.Event{Kind: agent.EventUsage, Usage: &agent.Usage{InputTokens: ctx, ContextTokens: ctx}})
	}
	if err := w.Close(agent.Result{Status: agent.StatusOK, Turns: 3, InputTokens: 90000, OutputTokens: 700, CacheRead: 1500, CacheWrite: 250}); err != nil {
		t.Fatal(err)
	}
	// A fixed start, so the row's day does not depend on when the test runs.
	rec, err := runlog.Load(w.ID())
	if err != nil {
		t.Fatal(err)
	}
	rec.Started = time.Date(2026, 9, 21, 12, 0, 0, 0, time.Local)
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runlog.Root(), w.ID(), "result.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return cmdReport([]string{"--by", "day"}) })
	if err != nil {
		t.Fatal(err)
	}
	const day = "2026-09-21"
	text := string(out)
	i := strings.Index(text, "tokens by day, all time\n")
	if !strings.HasPrefix(text, "runs by day, all time\n") || i < 0 {
		t.Fatalf("report =\n%s", text)
	}
	lines := strings.Split(strings.TrimRight(text[i:], "\n"), "\n")
	if len(lines) < 4 || !strings.HasPrefix(lines[1], "day ") || !strings.Contains(lines[1], "med context") {
		t.Fatalf("tokens table =\n%s", text[i:])
	}
	for _, row := range []string{lines[2], lines[3]} {
		f := strings.Fields(row)
		if len(f) != 8 || f[1] != "90.0k" || f[2] != "700" || f[3] != "1.5k" || f[4] != "250" || f[5] != "30.0k" || f[6] != "50.0k" || f[7] != "turns" {
			t.Errorf("row = %q, want input, output, cache read, cache write, median and peak context, and where they came from", row)
		}
	}
	if f := strings.Fields(lines[2]); f[0] != day {
		t.Errorf("first row is %q, want the run's day, %s", f[0], day)
	}
}
