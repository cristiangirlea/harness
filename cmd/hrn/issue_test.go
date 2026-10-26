package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/ws"
)

func TestRunRefusesAHeldIssueBeforeTheAgentStarts(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t)
	old := ws.GH
	ws.GH = func(context.Context, ...string) ([]byte, error) { return nil, errors.New("gh: not available in tests") }
	t.Cleanup(func() { ws.GH = old })
	held, err := ws.TakeIssue(context.Background(), runlog.Home(), ws.IssueRef{Repo: "o/n", Number: 5},
		ws.Lease{Holder: "hrn run", Run: "other", Backend: "codex"}, ws.IssueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	code, err := cmdRun([]string{"-b", "claude", "--no-switch", "--issue", "o/n#5", "-C", t.TempDir(), "fix", "it"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "run other") || !strings.Contains(err.Error(), "--take-over") {
		t.Fatalf("code=%d err=%v", code, err)
	}
	recs, _ := runlog.List(5)
	if len(recs) != 1 || !recs[0].Done || recs[0].Result.Status != agent.StatusError || recs[0].Result.Turns != 0 {
		t.Errorf("the refused run must be closed as an error without turns: %+v", recs)
	}
	if err := held.Release(context.Background(), ""); err != nil {
		t.Fatal(err)
	}

	if code, err := cmdRun([]string{"-b", "claude", "--no-switch", "--issue", "o/n#5", "-C", t.TempDir(), "fix", "it"}); code != 0 || err != nil {
		t.Fatalf("a free issue: code=%d err=%v", code, err)
	}
	if got, _ := ws.ReadIssues(runlog.Home()); len(got) != 0 {
		t.Errorf("the run's lease outlived it: %+v", got)
	}
	recs, _ = runlog.List(1)
	if len(recs) != 1 || recs[0].Task.Issue != "o/n#5" {
		t.Errorf("the run record must carry its issue: %+v", recs)
	}
}

func TestWSIssuesListsAsJSONAndFreesAnEndedHolder(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	old := ws.GH
	ws.GH = func(context.Context, ...string) ([]byte, error) { return nil, errors.New("gh: not available in tests") }
	t.Cleanup(func() { ws.GH = old })
	listIssues := func() []ws.IssueStatus {
		t.Helper()
		out, err := captureStdout(t, func() error {
			code, err := wsIssues(context.Background(), []string{"--json"})
			if code != 0 {
				t.Errorf("code = %d", code)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		var rows []ws.IssueStatus
		if err := json.Unmarshal(out, &rows); err != nil {
			t.Fatalf("not JSON: %v: %s", err, out)
		}
		return rows
	}
	if rows := listIssues(); rows == nil || len(rows) != 0 {
		t.Errorf("no leases must list as an empty array: %#v", rows)
	}
	if _, err := ws.TakeIssue(context.Background(), runlog.Home(), ws.IssueRef{Repo: "o/n", Number: 5},
		ws.Lease{Holder: "hrn run", Run: "r1", Backend: "codex", Branch: "fix-x"}, ws.IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	if rows := listIssues(); len(rows) != 1 || rows[0].Issue != "o/n#5" || rows[0].State != "held" || rows[0].Run != "r1" || rows[0].Backend != "codex" {
		t.Errorf("rows = %+v", rows)
	}
	if code, err := wsIssues(context.Background(), []string{"--free", "O/N#5"}); code != 1 || err == nil || !strings.Contains(err.Error(), "running process") {
		t.Errorf("a live holder must not be freed: code=%d err=%v", code, err)
	}
	// Ended without a release: the lease file stays, as after a crash.
	data, _ := os.ReadFile(filepath.Join(ws.IssuesDir(runlog.Home()), "o__n__5.lease"))
	var l ws.Lease
	_ = json.Unmarshal(data, &l)
	l.PID, l.Start, l.PR = 0, "", "https://github.com/o/n/pull/9"
	data, _ = json.Marshal(l)
	if err := os.WriteFile(filepath.Join(ws.IssuesDir(runlog.Home()), "o__n__5.lease"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, err := wsIssues(context.Background(), []string{"--free", "o/n#5"}); code != 0 || err != nil {
		t.Fatalf("free: code=%d err=%v", code, err)
	}
	if got, _ := ws.ReadIssues(runlog.Home()); len(got) != 0 {
		t.Errorf("free left the lease: %+v", got)
	}
	if code, err := wsIssues(context.Background(), []string{"--free", "o/n"}); code != 2 || err == nil {
		t.Errorf("a bad issue must be a usage error: code=%d err=%v", code, err)
	}
}
