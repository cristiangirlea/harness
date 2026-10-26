package hrnmcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/ws"
)

func TestRunToolRefusesAHeldIssueAsAToolError(t *testing.T) {
	s := connect(t, t.TempDir())
	standIn(t)
	old := ws.GH
	ws.GH = func(context.Context, ...string) ([]byte, error) { return nil, errors.New("gh: not available in tests") }
	t.Cleanup(func() { ws.GH = old })
	held, err := ws.TakeIssue(context.Background(), runlog.Home(), ws.IssueRef{Repo: "o/n", Number: 5},
		ws.Lease{Holder: "hrn run", Run: "other", Backend: "codex"}, ws.IssueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	res := s.call(t, "run", `{"prompt":"fix it","backend":"claude","no_switch":true,"issue":"o/n#5"}`)
	if !res.IsError || !strings.Contains(res.Text(), "run other") || !strings.Contains(res.Text(), "take_over") {
		t.Fatalf("want the refusal as a tool error: %s", res.Text())
	}
	if err := held.Release(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	res = s.call(t, "run", `{"prompt":"fix it","backend":"claude","no_switch":true,"issue":"o/n#5"}`)
	if res.IsError || !strings.Contains(res.Text(), "GitHub claim skipped") {
		t.Errorf("a free issue runs, noting the skipped claim: %s", res.Text())
	}
	if got, _ := ws.ReadIssues(runlog.Home()); len(got) != 0 {
		t.Errorf("the run's lease outlived it: %+v", got)
	}
}

func TestFlowToolIssuesCarryNoTerminalControls(t *testing.T) {
	got := cleanIssues([]ws.IssueStatus{{Issue: "o/n#5", Branch: "b\x1b]0;x\x07", Holder: "h\x9b2J"}})
	if strings.ContainsAny(got[0].Branch+got[0].Holder, "\x1b\x07\u009b") || got[0].Issue != "o/n#5" {
		t.Fatalf("cleanIssues = %+v", got[0])
	}
}
