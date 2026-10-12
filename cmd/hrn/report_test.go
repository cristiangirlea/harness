package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/report"
	"github.com/cristiangirlea/harness/internal/runlog"
)

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
