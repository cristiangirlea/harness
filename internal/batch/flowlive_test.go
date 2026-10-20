package batch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/flow"
	"github.com/cristiangirlea/harness/internal/owner"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestLiveFlowRetryAndGate(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "user:live-flow")
	dir := initRepo(t)
	res, err := Run(context.Background(), Spec{Dir: dir, Backend: "wb-bad", Retry: []string{"wb-ok"},
		Gate: []string{"exit 0"}, Tasks: []Task{{ID: "retry", Prompt: "first line\nmore"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	events, err := flow.Read(FlowPath(res.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("live flow is empty")
	}
	for i := 1; i < len(events); i++ {
		if events[i].Seq <= events[i-1].Seq {
			t.Fatalf("sequence is not increasing: %d then %d", events[i-1].Seq, events[i].Seq)
		}
	}
	if res.Owner != "user:live-flow" || events[0].Owner != res.Owner {
		t.Fatalf("owner = %q, first flow owner = %q", res.Owner, events[0].Owner)
	}
	tree := flow.Build(events)
	if len(tree) == 0 || tree[0].Step != BatchStep {
		t.Fatalf("flow tree root = %+v", tree)
	}
	want := map[string]flow.Status{BatchStep: flow.StatusOK, TaskStep("retry"): flow.StatusOK,
		AttemptStep("retry", 1): flow.StatusFailed, AttemptStep("retry", 2): flow.StatusOK,
		GateStep("retry", 2, 1): flow.StatusOK}
	seen := map[string]flow.Event{}
	for _, e := range events {
		seen[e.Step] = e
	}
	for step, status := range want {
		if got := seen[step].Status; got != status {
			t.Errorf("%s status = %q, want %q", step, got, status)
		}
	}
	if !strings.Contains(seen[TaskStep("retry")+"/route"].Reason, "wb-bad") {
		t.Errorf("route reason = %q", seen[TaskStep("retry")+"/route"].Reason)
	}
	retryDecision := false
	for _, e := range events {
		if e.Kind == flow.KindDecision && e.Parent == TaskStep("retry") && strings.Contains(e.Reason, "retrying on wb-ok") {
			retryDecision = true
		}
	}
	secondReason := ""
	for _, e := range events {
		if e.Step == AttemptStep("retry", 2) && e.Reason != "" {
			secondReason = e.Reason
		}
	}
	if !retryDecision || !strings.Contains(secondReason, "retry on wb-ok because") {
		t.Errorf("retry decision/reason missing: decision=%v reason=%q", retryDecision, secondReason)
	}
}

func TestLiveFlowFailingGate(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := initRepo(t)
	res, err := Run(context.Background(), Spec{Dir: dir, Backend: "wb-ok", Gate: []string{"exit 1"},
		Tasks: []Task{{ID: "badgate", Prompt: "run"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	events, err := flow.Read(FlowPath(res.ID))
	if err != nil {
		t.Fatal(err)
	}
	last := flow.Event{}
	for _, e := range events {
		if e.Step == GateStep("badgate", 1, 1) {
			last = e
		}
	}
	if last.Status != flow.StatusFailed || !strings.HasPrefix(last.Reason, "exited 1") {
		t.Fatalf("gate event = %+v", last)
	}
}

func TestLiveFlowParallelAndTimeline(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "user:parallel")
	dir := initRepo(t)
	res, err := Run(context.Background(), Spec{Dir: dir, Parallel: 2, Backend: "wb-ok", Tasks: []Task{
		{ID: "one", Prompt: "one"}, {ID: "two", Prompt: "two"},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	events, err := flow.Read(FlowPath(res.ID))
	if err != nil {
		t.Fatal(err)
	}
	seq := map[int64]bool{}
	for _, e := range events {
		if seq[e.Seq] {
			t.Fatalf("duplicate sequence %d", e.Seq)
		}
		seq[e.Seq] = true
	}
	for _, id := range []string{"one", "two"} {
		if !hasStep(events, TaskStep(id)) || !hasStep(events, AttemptStep(id, 1)) {
			t.Errorf("missing subtree for %s", id)
		}
	}
	timeline, err := Timeline(owner.LocalViewer(), res.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range res.Tasks {
		parent := AttemptStep(tr.ID, 1)
		if !hasParentStep(timeline, parent+"/run", parent) {
			t.Errorf("run not nested under %s", parent)
		}
	}
}

func TestLiveFlowCanceledBatch(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := initRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	go func() {
		<-blockStarted
		close(started)
		cancel()
	}()
	res, err := Run(ctx, Spec{Dir: dir, Backend: "wb-block", Tasks: []Task{{ID: "cut", Prompt: "wait"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	events, err := flow.Read(FlowPath(res.ID))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]flow.Status{}
	for _, e := range events {
		seen[e.Step] = e.Status
	}
	<-started
	if seen[TaskStep("cut")] != flow.StatusCanceled || seen[BatchStep] != flow.StatusCanceled {
		t.Fatalf("canceled statuses: task=%q batch=%q", seen[TaskStep("cut")], seen[BatchStep])
	}
}

func hasStep(events []flow.Event, step string) bool {
	for _, e := range events {
		if e.Step == step {
			return true
		}
	}
	return false
}

func hasParentStep(events []flow.Event, step, parent string) bool {
	for _, e := range events {
		if e.Step == step && e.Parent == parent {
			return true
		}
	}
	return false
}

func TestLiveFlowEndsAnAttemptOnlyAfterItsGates(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := initRepo(t)
	res, err := Run(context.Background(), Spec{Dir: dir, Backend: "wb-ok", Gate: []string{"exit 0", "exit 0"},
		Tasks: []Task{{ID: "gated", Prompt: "write"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	events, err := flow.Read(FlowPath(res.ID))
	if err != nil {
		t.Fatal(err)
	}
	var lastGate, firstEnd int64
	for _, e := range events {
		if strings.HasPrefix(e.Step, AttemptStep("gated", 1)+"/gate") && e.Status.Done() {
			lastGate = e.Seq
		}
		if e.Step == AttemptStep("gated", 1) && e.Status.Done() && firstEnd == 0 {
			firstEnd = e.Seq
		}
	}
	if lastGate == 0 || firstEnd < lastGate {
		t.Fatalf("attempt ended at seq %d before its last gate at %d: a live viewer would see it done while gates run", firstEnd, lastGate)
	}
}

func TestTheLiveFlowAndTheRebuildFromTheRecordAgree(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := initRepo(t)
	res, err := Run(context.Background(), Spec{Dir: dir, Backend: "wb-ok", Parallel: 1, Tasks: []Task{
		{ID: "plain", Prompt: "write", Gate: []string{"exit 0"}},
		{ID: "guarded", Prompt: "write", Backend: "wb-readme", Protect: []string{"README.md"}, ProtectMode: "fail"},
		{ID: "retried", Prompt: "write", Backend: "wb-bad", Retry: []string{"wb-ok"}, Gate: []string{"exit 1"}},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	live, err := flow.Read(FlowPath(res.ID))
	if err != nil {
		t.Fatal(err)
	}
	runs := map[string]runlog.Record{}
	livem := map[string]flow.Status{}
	for _, n := range flowNodes(flow.Build(live)) {
		livem[n.Step] = n.Status
		if n.Kind == flow.KindCommit && n.Status != flow.StatusOK {
			t.Errorf("commit step %s never ended: %q", n.Step, n.Status)
		}
	}
	for _, n := range flowNodes(flow.Build(FlowOf(res, runs))) {
		if got, ok := livem[n.Step]; !ok || got != n.Status {
			t.Errorf("step %s: rebuilt %q, live %q (recorded %v)", n.Step, n.Status, got, ok)
		}
	}
	if livem[AttemptStep("guarded", 1)+"/protect"] != flow.StatusFailed {
		t.Errorf("the protect check must be one failed step, live steps: %v", livem)
	}
}

func TestASlotWaitCanceledEndsItsStep(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := initRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rangeComputed = func(int) { cancel() }
	t.Cleanup(func() { rangeComputed = nil })
	res, _ := Run(ctx, Spec{Dir: dir, Backend: "wb-ok", Tasks: []Task{{ID: "waiting", Prompt: "write"}}}, nil)
	if res.ID == "" {
		t.Fatal("the batch never started")
	}
	events, err := flow.Read(FlowPath(res.ID))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range flowNodes(flow.Build(events)) {
		if n.Step == SlotStep("waiting") {
			if n.Status != flow.StatusCanceled || n.Next != "" {
				t.Fatalf("slot step = %+v: a canceled wait must end the step, not leave it waiting", n)
			}
			return
		}
	}
	t.Fatal("no slot step was recorded")
}

func init() {
	agent.Register(stub{"wb-readme", func(_ context.Context, task agent.Task) agent.Result {
		_ = os.WriteFile(filepath.Join(task.Dir, "README.md"), []byte("changed"), 0o644)
		return agent.Result{Backend: "wb-readme", Status: agent.StatusOK, Final: "changed the readme", Turns: 1}
	}})
}

func flowNodes(roots []*flow.Node) []*flow.Node {
	var out []*flow.Node
	flow.Walk(roots, func(n *flow.Node, _ int) { out = append(out, n) })
	return out
}

func TestAUserGateNamedProtectEndsAndAWarnOnlyGuardIsInfo(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := initRepo(t)
	res, err := Run(context.Background(), Spec{Dir: dir, Backend: "wb-readme", Protect: []string{"README.md"},
		Gate: []string{"protect check || exit 0"}, Tasks: []Task{{ID: "warned", Prompt: "write"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	live, err := flow.Read(FlowPath(res.ID))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range flowNodes(flow.Build(live)) {
		if n.Kind == flow.KindGate && !n.Status.Done() {
			t.Errorf("gate step %s never ended", n.Step)
		}
	}
	steps := map[string]*flow.Node{}
	for _, n := range flowNodes(flow.Build(live)) {
		steps[n.Step] = n
	}
	info := steps[AttemptStep("warned", 1)+"/protected"]
	if info == nil || info.Kind != flow.KindInfo || info.Status != flow.StatusOK || info.Text != "README.md" {
		t.Fatalf("a guard that only reports must record an info step, got %+v", info)
	}
	for _, n := range flowNodes(flow.Build(FlowOf(res, nil))) {
		if live := steps[n.Step]; live == nil || live.Status != n.Status {
			t.Errorf("rebuilt step %s (%s) differs from the live flow", n.Step, n.Status)
		}
	}
}
