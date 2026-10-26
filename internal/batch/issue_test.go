package batch

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/ws"
)

// noGitHub makes every gh call fail, so the leases stay local to the test's home.
func noGitHub(t *testing.T) {
	t.Helper()
	old := ws.GH
	ws.GH = func(context.Context, ...string) ([]byte, error) { return nil, errors.New("gh: not available in tests") }
	t.Cleanup(func() { ws.GH = old })
}

func TestTwoTasksForOneIssueAreRefusedBeforeAnyStarts(t *testing.T) {
	dir := initRepo(t)
	noGitHub(t)
	_, err := Run(context.Background(), Spec{Dir: dir, Backend: "wb-ok", Tasks: []Task{
		{ID: "a", Prompt: "p", Options: agent.Options{Issue: "o/n#7"}},
		{ID: "b", Prompt: "p", Options: agent.Options{Issue: "https://github.com/o/n/issues/7"}},
	}}, nil)
	if err == nil || !strings.Contains(err.Error(), "a and b") || !strings.Contains(err.Error(), "o/n#7") {
		t.Fatalf("err = %v", err)
	}
	if got, _ := ws.ReadIssues(runlog.Home()); len(got) != 0 {
		t.Errorf("a refused spec took leases: %+v", got)
	}
}

func TestOneIssueInTwoCasingsIsOneIssue(t *testing.T) {
	for _, takeOver := range []bool{false, true} {
		dir := initRepo(t)
		noGitHub(t)
		_, err := Run(context.Background(), Spec{Dir: dir, Backend: "wb-ok", Options: agent.Options{TakeOver: takeOver}, Tasks: []Task{
			{ID: "a", Prompt: "p", Options: agent.Options{Issue: "O/N#12"}},
			{ID: "b", Prompt: "p", Options: agent.Options{Issue: "o/n#12"}},
		}}, nil)
		if err == nil || !strings.Contains(err.Error(), "a and b") || !strings.Contains(err.Error(), "o/n#12") {
			t.Errorf("take_over %t: err = %v", takeOver, err)
		}
		if got, _ := ws.ReadIssues(runlog.Home()); len(got) != 0 {
			t.Errorf("take_over %t: a refused spec took leases: %+v", takeOver, got)
		}
	}
}

func TestABatchLeaseNamesOnlyAnExplicitBackend(t *testing.T) {
	dir := initRepo(t)
	noGitHub(t)
	spec := Spec{Dir: dir, Tasks: []Task{
		{ID: "routed", Prompt: "p", Options: agent.Options{Issue: "o/n#1"}},
		{ID: "chosen", Prompt: "p", Backend: "codex", Options: agent.Options{Issue: "o/n#2"}},
	}}
	leases, err := takeIssues(context.Background(), &spec, dir, "b1", func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	defer releaseIssues(context.Background(), leases, "test ended", func(Event) {}, spec.Tasks)
	got, _ := ws.ReadIssues(runlog.Home())
	if len(got) != 2 || got[0].Run != "b1/routed" || got[0].Backend != "" || got[1].Run != "b1/chosen" || got[1].Backend != "codex" {
		t.Errorf("leases = %+v", got)
	}
}

func TestABatchWhoseRecordCannotBeSavedGivesItsIssuesBack(t *testing.T) {
	dir := initRepo(t)
	noGitHub(t)
	failSave = func(Result) error { return errors.New("disk full") }
	t.Cleanup(func() { failSave = nil })
	_, err := Run(context.Background(), Spec{Dir: dir, Backend: "wb-ok", Tasks: []Task{
		{ID: "a", Prompt: "p", Options: agent.Options{Issue: "o/n#1"}},
	}}, nil)
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v", err)
	}
	if got, _ := ws.ReadIssues(runlog.Home()); len(got) != 0 {
		t.Errorf("a batch that never started kept its leases: %+v", got)
	}
}

func TestACanceledBatchGivesItsIssuesBack(t *testing.T) {
	dir := initRepo(t)
	noGitHub(t)
	ctx, cancel := context.WithCancel(context.Background())
	var during []ws.IssueStatus
	go func() {
		<-blockStarted
		during, _ = ws.ReadIssues(runlog.Home())
		cancel()
	}()
	res, err := Run(ctx, Spec{Dir: dir, Parallel: 1, Backend: "wb-block", Tasks: []Task{
		{ID: "a", Prompt: "p", Options: agent.Options{Issue: "o/n#1"}},
		{ID: "b", Prompt: "p", Options: agent.Options{Issue: "o/n#2"}},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(during) != 2 {
		t.Errorf("both leases must be taken before the first task runs: %+v", during)
	}
	if res.Tasks[0].Status != StatusCanceled || res.Tasks[1].Status != StatusCanceled {
		t.Errorf("tasks = %+v", res.Tasks)
	}
	if got, _ := ws.ReadIssues(runlog.Home()); len(got) != 0 {
		t.Errorf("a canceled batch kept its leases: %+v", got)
	}
}

func TestAHeldIssueRefusesTheBatchAndGivesBackTheLeasesItTook(t *testing.T) {
	dir := initRepo(t)
	noGitHub(t)
	held, err := ws.TakeIssue(context.Background(), runlog.Home(), ws.IssueRef{Repo: "o/n", Number: 2},
		ws.Lease{Holder: "hrn run", Run: "other", Backend: "codex"}, ws.IssueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release(context.Background(), "")
	_, err = Run(context.Background(), Spec{Dir: dir, Backend: "wb-ok", Tasks: []Task{
		{ID: "a", Prompt: "p", Options: agent.Options{Issue: "o/n#1"}},
		{ID: "b", Prompt: "p", Options: agent.Options{Issue: "o/n#2"}},
	}}, nil)
	var refused *ws.IssueHeldError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "task b") || !strings.Contains(err.Error(), "run other") {
		t.Fatalf("err = %v", err)
	}
	got, _ := ws.ReadIssues(runlog.Home())
	if len(got) != 1 || got[0].Issue != "o/n#2" || got[0].Run != "other" {
		t.Errorf("the batch must give back the lease it took and leave the holder's: %+v", got)
	}
}

func TestATaskHoldsItsIssueWhileItRunsAndGivesItBackAfter(t *testing.T) {
	dir := initRepo(t)
	noGitHub(t)
	var during []ws.IssueStatus
	setHook(t, func(agent.Task) { during, _ = ws.ReadIssues(runlog.Home()) })
	res, events := run(t, Spec{Dir: dir, Backend: "wb-hook", Options: agent.Options{Issue: "o/n#3"}, Tasks: []Task{{ID: "fix", Prompt: "p"}}})
	if len(during) != 1 || during[0].Run != res.ID+"/fix" || during[0].Branch != "hrn/fix" || during[0].Backend != "wb-hook" || during[0].State != "held" {
		t.Errorf("lease while the task ran = %+v", during)
	}
	if after, _ := ws.ReadIssues(runlog.Home()); len(after) != 0 {
		t.Errorf("the task's lease outlived it: %+v", after)
	}
	if notes := eventTexts(events, "issue"); !strings.Contains(notes, "GitHub claim skipped") {
		t.Errorf("issue notes = %q", notes)
	}
}
