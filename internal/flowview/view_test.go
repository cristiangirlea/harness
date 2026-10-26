package flowview

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/flow"
	"github.com/cristiangirlea/harness/internal/owner"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/ws"
)

func TestFilterAndWriter(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	root := []*flow.Node{{Kind: flow.KindRun, Run: "r", Status: flow.StatusOK, Children: []*flow.Node{{Kind: flow.KindTool, Name: "shell", Reason: "failed"}}}}
	if got := Filter(root, "simple"); len(got) != 1 || len(got[0].Children) != 0 {
		t.Fatalf("simple tree = %#v", got)
	}
	var b strings.Builder
	if err := WriteText(&b, RunSummary(root), nil, Filter(root, "technical"), "technical"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "why: failed") {
		t.Fatalf("text = %q", b.String())
	}
}

func TestLoadRunAndBatchSummaries(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:one")
	w, err := runlog.Create("stub", agent.Task{Prompt: "run prompt"})
	if err != nil {
		t.Fatal(err)
	}
	w.Event(agent.Event{Kind: agent.EventToolCall, Tool: "shell", Text: "go test"})
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	got, err := Load(owner.LocalViewer(), w.ID())
	if err != nil || !strings.Contains(got.Summary, "run "+w.ID()+": ok") || len(got.Events) == 0 {
		t.Fatalf("run view = %+v err=%v", got, err)
	}
	batchID := "20261021-120000-abcdef"
	res := batch.Result{ID: batchID, Owner: "local:one", Started: time.Now(), Tasks: []batch.TaskResult{{ID: "task", Status: batch.StatusFail, Attempts: []batch.Attempt{{Gates: []batch.GateResult{{Command: "go test ./...", ExitCode: 1}}}}}}}
	if err := batch.SaveResult(res); err != nil {
		t.Fatal(err)
	}
	got, err = Load(owner.LocalViewer(), batchID)
	if err != nil || !strings.Contains(got.Summary, "gate go test ./...: exited 1") || len(got.Tree) == 0 {
		t.Fatalf("batch view = %+v err=%v", got, err)
	}
	if _, err := Load(owner.Viewer{ID: "local:two"}, w.ID()); err == nil {
		t.Fatal("loaded another owner's run")
	}
}

func TestLoadCleansStoredFlowAgain(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:one")
	id := "20261021-120000-abcdef"
	if err := batch.SaveResult(batch.Result{ID: id, Owner: "local:one", Started: time.Now()}); err != nil {
		t.Fatal(err)
	}
	line := `{"v":1,"seq":1,"step":"batch","kind":"batch","text":"ghp_abcdefghijklmnop1234"}` + "\n"
	if err := os.MkdirAll(filepath.Dir(batch.FlowPath(id)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(batch.FlowPath(id), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(owner.LocalViewer(), id)
	if err != nil || strings.Contains(fmt.Sprint(got.Tree), "ghp_abcdefghijklmnop1234") {
		t.Fatalf("view = %+v err=%v", got, err)
	}
}

func TestWriteTextKeepsItsMarksAndDashes(t *testing.T) {
	tree := []*flow.Node{{Step: "run:r", Kind: flow.KindRun, Name: "codex", Status: flow.StatusOK, Text: "done", Children: []*flow.Node{
		{Step: "run:r/tool1", Kind: flow.KindTool, Name: "shell", Status: flow.StatusFailed},
		{Step: "run:r/tool2", Kind: flow.KindTool, Name: "read", Status: flow.StatusStarted},
	}}}
	var b strings.Builder
	if err := WriteText(&b, "summary", nil, tree, "technical"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"✓ run codex — done", "✗ tool shell", "… tool read"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in: %s", want, b.String())
		}
	}
}

func TestLoadShowsTheIssuesItsRunOrBatchHolds(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:one")
	w, err := runlog.Create("stub", agent.Task{Prompt: "run prompt"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	batchID := "20261021-120000-abcdef"
	if err := batch.SaveResult(batch.Result{ID: batchID, Owner: "local:one", Started: time.Now()}); err != nil {
		t.Fatal(err)
	}
	take := func(n int, l ws.Lease) {
		t.Helper()
		il, err := ws.TakeIssue(t.Context(), runlog.Home(), ws.IssueRef{Repo: "o/n", Number: n}, l, ws.IssueOptions{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { il.Release(t.Context(), "") })
	}
	take(1, ws.Lease{Holder: "hrn run", Run: w.ID(), Backend: "codex", Branch: "fix-x"})
	take(2, ws.Lease{Holder: "hrn batch", Run: batchID + "/t1", Backend: "claude", Branch: "hrn/t1"})
	take(3, ws.Lease{Holder: "hrn run", Run: "someone-else"})

	run, err := Load(owner.LocalViewer(), w.ID())
	if err != nil || len(run.Issues) != 1 || run.Issues[0].Issue != "o/n#1" {
		t.Fatalf("run issues = %+v err=%v", run.Issues, err)
	}
	var b strings.Builder
	if err := WriteText(&b, run.Summary, run.Issues, nil, "technical"); err != nil {
		t.Fatal(err)
	}
	if want := "issue o/n#1: held by hrn " + w.ID() + " (codex) on fix-x since "; !strings.Contains(b.String(), want) {
		t.Errorf("text %q lacks %q", b.String(), want)
	}
	got, err := Load(owner.LocalViewer(), batchID)
	if err != nil || len(got.Issues) != 1 || got.Issues[0].Run != batchID+"/t1" {
		t.Fatalf("batch issues = %+v err=%v", got.Issues, err)
	}
	if got := IssueLine(ws.IssueStatus{Issue: "o/n#4", State: "held by pull request", PR: "https://github.com/o/n/pull/9"}); got != "issue o/n#4: held by pull request https://github.com/o/n/pull/9" {
		t.Errorf("pull request line = %q", got)
	}
}

func TestACarriedOnRunShowsTheIssueItsFirstRunTookRedacted(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:one")
	prev := ""
	var ids []string
	for _, backend := range []string{"claude", "codex", "claude"} {
		w, err := runlog.CreateContinuing(backend, agent.Task{Prompt: "p"}, prev)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Close(agent.Result{Backend: backend, Status: agent.StatusOK}); err != nil {
			t.Fatal(err)
		}
		prev = w.ID()
		ids = append(ids, prev)
	}
	secret := "supersecretvalue123"
	il, err := ws.TakeIssue(t.Context(), runlog.Home(), ws.IssueRef{Repo: "o/n", Number: 1},
		ws.Lease{Holder: "hrn run", Run: ids[0], Backend: "claude", Branch: "API_TOKEN=" + secret}, ws.IssueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { il.Release(t.Context(), "") })
	for _, id := range ids {
		got, err := Load(owner.LocalViewer(), id)
		if err != nil || len(got.Issues) != 1 || got.Issues[0].Issue != "o/n#1" {
			t.Fatalf("run %s issues = %+v err=%v", id, got.Issues, err)
		}
		var b strings.Builder
		if err := WriteText(&b, got.Summary, got.Issues, nil, "technical"); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(fmt.Sprint(got.Issues), secret) || strings.Contains(b.String(), secret) {
			t.Errorf("run %s shows the secret in its issue lines: %s", id, b.String())
		}
	}
}

func closedRun(t *testing.T, continues string) string {
	t.Helper()
	w, err := runlog.CreateContinuing("claude", agent.Task{Prompt: "p"}, continues)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	return w.ID()
}

func takeFor(t *testing.T, n int, run string) {
	t.Helper()
	il, err := ws.TakeIssue(t.Context(), runlog.Home(), ws.IssueRef{Repo: "o/n", Number: n}, ws.Lease{Holder: "hrn run", Run: run}, ws.IssueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { il.Release(t.Context(), "") })
}

func issueNames(s []ws.IssueStatus) []string {
	var out []string
	for _, i := range s {
		out = append(out, i.Issue)
	}
	return out
}

func TestAContinuationShowsNoIssueOfAPredecessorTheViewerMayNotLoad(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:two")
	foreign := closedRun(t, "")
	t.Setenv("HRN_OWNER", "local:one")
	mine := closedRun(t, foreign)
	takeFor(t, 1, foreign)
	got, err := Load(owner.LocalViewer(), mine)
	if err != nil || len(got.Issues) != 0 {
		t.Fatalf("issues = %v, err = %v; another owner's run must show nothing", issueNames(got.Issues), err)
	}
}

func TestTheContinuationWalkEndsAtACycleAMissingRunAndTwentySteps(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:one")

	first := closedRun(t, "")
	second := closedRun(t, first)
	p := filepath.Join(runlog.Root(), first, "result.json")
	var rec map[string]any
	data, err := os.ReadFile(p)
	if err == nil {
		dec := json.NewDecoder(strings.NewReader(string(data)))
		dec.UseNumber()
		err = dec.Decode(&rec)
	}
	if err != nil {
		t.Fatal(err)
	}
	rec["continues"] = second
	if data, err = json.Marshal(rec); err != nil || os.WriteFile(p, data, 0o644) != nil {
		t.Fatal("rewriting the record failed")
	}
	takeFor(t, 1, first)
	if got, err := Load(owner.LocalViewer(), second); err != nil || !slices.Equal(issueNames(got.Issues), []string{"o/n#1"}) {
		t.Errorf("cycle: issues = %v, err = %v", issueNames(got.Issues), err)
	}

	gone := "20200101-000000-abcdef"
	orphan := closedRun(t, gone)
	takeFor(t, 2, gone)
	if got, err := Load(owner.LocalViewer(), orphan); err != nil || len(got.Issues) != 0 {
		t.Errorf("missing predecessor: issues = %v, err = %v", issueNames(got.Issues), err)
	}

	chain := []string{closedRun(t, "")}
	for range maxContinues + 1 {
		chain = append(chain, closedRun(t, chain[len(chain)-1]))
	}
	takeFor(t, 3, chain[0])
	takeFor(t, 4, chain[1])
	if got, err := Load(owner.LocalViewer(), chain[len(chain)-1]); err != nil || !slices.Equal(issueNames(got.Issues), []string{"o/n#4"}) {
		t.Errorf("twenty steps: issues = %v, err = %v; want only the issue of the twentieth predecessor", issueNames(got.Issues), err)
	}
}
