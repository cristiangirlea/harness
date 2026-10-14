package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/eval"
	"github.com/cristiangirlea/harness/internal/route"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func routeOutput(t *testing.T, args ...string) (string, int, error) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	old := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = old }()
	code, err := cmdRoute(args)
	data, readErr := os.ReadFile(f.Name())
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(data), code, err
}

func TestRouteShowAndSet(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	if out, code, err := routeOutput(t, "show"); code != 0 || err != nil || !strings.Contains(out, "(empty)") {
		t.Fatalf("empty: %s %d %v", out, code, err)
	}
	for _, args := range [][]string{{"set", "review", "-b", "claude", "-m", "opus", "--effort", "high"}, {"set", "implement", "--model", "sol"}, {"set", "debug", "--backend", "codex"}, {"set", "test", "--effort", "low"}} {
		if _, code, err := routeOutput(t, args...); code != 0 || err != nil {
			t.Fatalf("set: %v %d %v", args, code, err)
		}
	}
	tab, err := route.Load()
	if err != nil || len(tab) != 4 || tab["review"].Backend != "claude" || tab["review"].Effort != "high" || tab["review"].Eval != "" || tab["review"].Set != time.Now().Format("2006-01-02") {
		t.Fatalf("table: %+v %v", tab, err)
	}
	out, code, err := routeOutput(t, "show")
	if code != 0 || err != nil {
		t.Fatal(code, err)
	}
	for _, s := range []string{"review", "claude", "opus", "high", "hand", "implement", "sol"} {
		if !strings.Contains(out, s) {
			t.Errorf("show lacks %s: %s", s, out)
		}
	}
	if strings.Index(out, "debug") > strings.Index(out, "review") {
		t.Fatalf("unsorted: %s", out)
	}
	if _, code, err := routeOutput(t, "set", "review", "--effort", "low"); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	tab, _ = route.Load()
	if tab["review"].Backend != "" || tab["review"].Model != "" || tab["review"].Effort != "low" {
		t.Fatalf("replacement: %+v", tab["review"])
	}
}

func TestRouteBadArgumentsDoNotSave(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, args := range [][]string{nil, {"wat"}, {"show", "extra"}, {"set"}, {"set", "review"}, {"set", "bogus", "-b", "codex"}, {"set", "review", "-b", "bogus"}, {"set", "review", "--effort", "turbo"}, {"set", "review", "--wat"}, {"set", "review", "-m", "x", "extra"}, {"from-eval"}, {"from-eval", "id", "extra"}, {"from-eval", "id", "--margin", "-1"}, {"from-eval", "id", "--margin", "2"}, {"from-eval", "id", "--margin", "NaN"}, {"from-eval", "id", "--margin", "Inf"}, {"from-eval", "missing"}} {
		if _, code, err := routeOutput(t, args...); code == 0 || err == nil {
			t.Errorf("accepted %v: %d %v", args, code, err)
		}
	}
	if _, err := os.Stat(filepath.Join(runlog.Home(), "route.json")); !os.IsNotExist(err) {
		t.Fatalf("saved on error: %v", err)
	}
}

func TestRouteFromEvalPrintsAndMerges(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	if err := route.Save(route.Table{"review": {Backend: "claude"}}); err != nil {
		t.Fatal(err)
	}
	r := eval.Record{ID: "fixture", Started: time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC), Spec: eval.Spec{Spec: batch.Spec{Tasks: []batch.Task{{ID: "one", Prompt: "Implement a widget"}}}, Matrix: []eval.Variant{{Name: "best", Backend: "codex", Model: "sol", Effort: "high"}, {Name: "cheap", Backend: "codex", Model: "sol", Effort: "low"}}}, Table: eval.Table{Variants: []eval.VariantRow{{Name: "best", MedianCostUSD: 2}, {Name: "cheap", MedianCostUSD: 1}}, Tasks: []eval.TaskRow{{Task: "one", Variant: "best", Passed: 10, Total: 10}, {Task: "one", Variant: "cheap", Passed: 9, Total: 10}}}}
	dir := filepath.Join(runlog.Home(), "evals")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(r)
	if err := os.WriteFile(filepath.Join(dir, "fixture.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args   []string
		effort string
	}{{[]string{"from-eval", "fixture"}, "high"}, {[]string{"from-eval", "fixture", "--margin", "0.1"}, "low"}} {
		out, code, err := routeOutput(t, tc.args...)
		if code != 0 || err != nil || !strings.Contains(out, "implement") || !strings.Contains(out, tc.effort) || !strings.Contains(out, "fixture") {
			t.Fatalf("from eval: %s %d %v", out, code, err)
		}
		tab, err := route.Load()
		if err != nil || tab["implement"].Effort != tc.effort || tab["implement"].Eval != "fixture" || tab["review"].Backend != "claude" {
			t.Fatalf("table: %+v %v", tab, err)
		}
	}
}
