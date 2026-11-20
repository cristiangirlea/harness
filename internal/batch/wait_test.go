package batch

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/flow"
	"github.com/cristiangirlea/harness/internal/followup"
	"github.com/cristiangirlea/harness/internal/limits"
)

// backingOff turns the backoff on for one test.
func backingOff(t *testing.T) {
	t.Helper()
	orig := maxBackoffs
	maxBackoffs = 3
	t.Cleanup(func() { maxBackoffs = orig })
}

// recordBackoff turns the backoff on for one test, replacing its sleep to record each delay.
func recordBackoff(t *testing.T) *[]time.Duration {
	t.Helper()
	backingOff(t)
	var delays []time.Duration
	orig := backoffSleep
	backoffSleep = func(ctx context.Context, d time.Duration) error {
		delays = append(delays, d)
		return ctx.Err()
	}
	t.Cleanup(func() { backoffSleep = orig })
	return &delays
}

// flaky ends with an error before any gate the first fails times, then writes its file and passes.
func flaky(t *testing.T, fails int32) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	orig, _ := agent.Get("wb-ok")
	agent.Register(stub{"wb-flaky", func(ctx context.Context, task agent.Task) agent.Result {
		if calls.Add(1) <= fails {
			return agent.Result{Backend: "wb-flaky", Status: agent.StatusError, Error: "connection reset"}
		}
		return orig.Run(ctx, task, func(agent.Event) {})
	}})
	return &calls
}

func TestAnInfrastructureErrorIsBackedOffAndRetriedOnTheSameBackend(t *testing.T) {
	dir := initRepo(t)
	delays := recordBackoff(t)
	calls := flaky(t, 1)
	res, events := run(t, Spec{Dir: dir, Backend: "wb-flaky", Retry: []string{"wb-ok"}, Gate: []string{"test -f out.txt"},
		Tasks: []Task{{ID: "net", Prompt: "write it"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusPass || len(tr.Attempts) != 2 || calls.Load() != 2 {
		t.Fatalf("task = %s, %d attempts, %d calls; want the error retried once on wb-flaky", tr.Status, len(tr.Attempts), calls.Load())
	}
	a1, a2 := tr.Attempts[0], tr.Attempts[1]
	if a1.Backend != "wb-flaky" || a2.Backend != "wb-flaky" || a1.Backoff != 0 || a2.Backoff != agent.Duration(backoffBase) {
		t.Fatalf("attempts: %s backoff %s, %s backoff %s", a1.Backend, a1.Backoff, a2.Backend, a2.Backoff)
	}
	if len(*delays) != 1 || (*delays)[0] != backoffBase {
		t.Fatalf("delays = %v, want one of %s", *delays, backoffBase)
	}
	if info := eventTexts(events, "info"); !strings.Contains(info, "retrying on wb-flaky after "+backoffBase.String()) {
		t.Errorf("info events = %q, want the backoff named", info)
	}
	if reason := attemptReason(tr.Attempts, 1); !strings.Contains(reason, "backoff") {
		t.Errorf("attempt 2 reason = %q, want the backoff said", reason)
	}
}

func TestBackoffDoublesUpToItsLimitThenTheRetryListMovesOn(t *testing.T) {
	dir := initRepo(t)
	delays := recordBackoff(t)
	flaky(t, 100)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-flaky", Retry: []string{"wb-ok"}, Tasks: []Task{{ID: "down", Prompt: "write it"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusPass || len(tr.Attempts) != 5 {
		t.Fatalf("task = %s with %d attempts, want 4 on wb-flaky and then wb-ok", tr.Status, len(tr.Attempts))
	}
	want := []time.Duration{backoffBase, 2 * backoffBase, 4 * backoffBase}
	if len(*delays) != len(want) {
		t.Fatalf("delays = %v, want %v", *delays, want)
	}
	for i := range want {
		if (*delays)[i] != min(want[i], backoffCap) {
			t.Fatalf("delays = %v, want %v capped at %s", *delays, want, backoffCap)
		}
	}
	if last := tr.Attempts[len(tr.Attempts)-1]; last.Backend != "wb-ok" || last.Backoff != 0 {
		t.Fatalf("last attempt: %s backoff %s", last.Backend, last.Backoff)
	}
}

func TestAGateFailureIsNotBackedOff(t *testing.T) {
	dir := initRepo(t)
	delays := recordBackoff(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-ok", Retry: []string{"wb-ok"}, Gate: []string{"exit 1"},
		Tasks: []Task{{ID: "gate", Prompt: "write it"}}})
	if tr := res.Tasks[0]; tr.Status != StatusFail || len(tr.Attempts) != 2 || len(*delays) != 0 {
		t.Fatalf("task = %s, %d attempts, delays %v: a failing gate is retried by the list, not backed off", tr.Status, len(tr.Attempts), *delays)
	}
}

func TestACanceledBackoffCancelsTheTask(t *testing.T) {
	dir := initRepo(t)
	backingOff(t)
	flaky(t, 100)
	ctx, cancel := context.WithCancel(context.Background())
	orig := backoffSleep
	backoffSleep = func(ctx context.Context, d time.Duration) error { cancel(); return orig(ctx, time.Hour) }
	t.Cleanup(func() { backoffSleep = orig })
	res, err := Run(ctx, Spec{Dir: dir, Backend: "wb-flaky", Tasks: []Task{{ID: "stop", Prompt: "write it"}}}, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if tr := res.Tasks[0]; tr.Status != StatusCanceled || len(tr.Attempts) != 1 {
		t.Fatalf("task = %s with %d attempts, want canceled during the backoff", tr.Status, len(tr.Attempts))
	}
}

func TestAnAttemptWaitsForTheResetInsteadOfSkipping(t *testing.T) {
	dir := initRepo(t)
	standIn(t)
	slept := fakeWait(t)
	agent.Register(stub{"codex", func(context.Context, agent.Task) agent.Result {
		return agent.Result{Backend: "codex", Status: agent.StatusOK, Final: "finished", Turns: 1}
	}})
	for backend, in := range map[string]time.Duration{"codex": 20 * time.Second, "claude": time.Hour} {
		if err := limits.Note(backend, agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(in)}); err != nil {
			t.Fatal(err)
		}
	}
	res, events := run(t, Spec{Dir: dir, Backend: "codex", Options: agent.Options{WaitLimit: agent.Duration(time.Minute)},
		Tasks: []Task{{ID: "later", Prompt: "write it"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusPass || len(tr.Attempts) != 1 || tr.Attempts[0].Backend != "codex" {
		t.Fatalf("task = %+v, want one codex attempt after its reset", tr)
	}
	if info := eventTexts(events, "info"); !strings.Contains(info, "waiting for codex's usage limit to reset") {
		t.Errorf("info events = %q, want the wait named", info)
	}
	waited := false
	for _, e := range mustTimeline(t, res.ID) {
		waited = waited || e.Kind == flow.KindDecision && e.Status == flow.StatusWaiting && e.Name == "codex" && strings.Contains(e.Reason, "reset at")
	}
	if !waited {
		t.Error("no waiting flow step names codex and its reset")
	}
	if len(*slept) != 1 {
		t.Errorf("slept %v, want one wait for codex's reset", *slept)
	}
}

func TestWithoutWaitLimitABothOutAttemptIsSkipped(t *testing.T) {
	dir := initRepo(t)
	standIn(t)
	for _, backend := range []string{"codex", "claude"} {
		if err := limits.Note(backend, agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	res, _ := run(t, Spec{Dir: dir, Backend: "codex", Tasks: []Task{{ID: "now", Prompt: "write it"}}})
	if tr := res.Tasks[0]; tr.Status != StatusFail || len(tr.Attempts) != 0 || !strings.Contains(tr.Error, "usage limit") {
		t.Fatalf("task = %s, %d attempts, %q: without wait_limit it is not started", tr.Status, len(tr.Attempts), tr.Error)
	}
}

// fakeWait replaces the reset wait's sleep for one test and records what it was asked for.
func fakeWait(t *testing.T) *[]time.Duration {
	t.Helper()
	var slept []time.Duration
	orig := followup.WaitSleep
	followup.WaitSleep = func(ctx context.Context, d time.Duration) error {
		slept = append(slept, d)
		return ctx.Err()
	}
	t.Cleanup(func() { followup.WaitSleep = orig })
	return &slept
}

func TestALimitedAttemptWaitsForTheResetAndCarriesOn(t *testing.T) {
	for name, c := range map[string]struct {
		setup func(t *testing.T)
		opts  agent.Options
	}{
		"partner limited too": {func(t *testing.T) {
			if err := limits.Note("claude", agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
		}, agent.Options{}},
		"partner would bill per token": {func(t *testing.T) { t.Setenv("ANTHROPIC_API_KEY", "fake-secret-value") }, agent.Options{}},
		"no_switch":                    {func(*testing.T) {}, agent.Options{NoSwitch: true}},
	} {
		t.Run(name, func(t *testing.T) {
			dir := initRepo(t)
			standIn(t)
			slept := fakeWait(t)
			var calls atomic.Int32
			orig, _ := agent.Get("wb-ok")
			agent.Register(stub{"codex", func(ctx context.Context, task agent.Task) agent.Result {
				if calls.Add(1) == 1 {
					return agent.Result{Backend: "codex", Status: agent.StatusLimited, Error: "You've hit your usage limit.", LimitResets: time.Now().Add(20 * time.Second)}
				}
				res := orig.Run(ctx, task, func(agent.Event) {})
				res.Backend = "codex"
				return res
			}})
			c.setup(t)
			c.opts.WaitLimit = agent.Duration(time.Minute)
			res, _ := run(t, Spec{Dir: dir, Backend: "codex", Options: c.opts, Tasks: []Task{{ID: "after", Prompt: "write it"}}})
			tr := res.Tasks[0]
			if tr.Status != StatusPass || len(tr.Attempts) != 2 || tr.Attempts[1].Backend != "codex" || tr.Attempts[1].SwitchedFrom != "codex" {
				t.Fatalf("task = %s with %d attempts (%q), want a limited codex attempt carried on on codex after its reset", tr.Status, len(tr.Attempts), tr.Error)
			}
			if len(*slept) != 1 || (*slept)[0] > time.Minute {
				t.Errorf("slept %v, want one wait for codex's reset", *slept)
			}
		})
	}
}

func TestASingleAttemptSpecNeitherBacksOffNorWaits(t *testing.T) {
	dir := initRepo(t)
	delays := recordBackoff(t)
	slept := fakeWait(t)
	flaky(t, 100)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-flaky", SingleAttempt: true, Options: agent.Options{WaitLimit: agent.Duration(time.Minute)},
		Tasks: []Task{{ID: "cell", Prompt: "write it"}}})
	if tr := res.Tasks[0]; len(tr.Attempts) != 1 || len(*delays) != 0 || len(*slept) != 0 {
		t.Fatalf("%d attempts, backoffs %v, waits %v: an eval cell is one attempt", len(tr.Attempts), *delays, *slept)
	}
}

func TestATaskWaitLimitShorterThanTheResetSkipsTheAttempt(t *testing.T) {
	dir := initRepo(t)
	standIn(t)
	slept := fakeWait(t)
	for b, in := range map[string]time.Duration{"codex": 2 * time.Hour, "claude": 3 * time.Hour} {
		if err := limits.Note(b, agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(in)}); err != nil {
			t.Fatal(err)
		}
	}
	res, _ := run(t, Spec{Dir: dir, Backend: "codex",
		Tasks: []Task{{ID: "later", Prompt: "write it", Options: agent.Options{WaitLimit: agent.Duration(time.Minute)}}}})
	if tr := res.Tasks[0]; tr.Status != StatusFail || len(tr.Attempts) != 0 || !strings.Contains(tr.Error, "not waiting") || len(*slept) != 0 {
		t.Fatalf("task = %s, %d attempts, %q, slept %v: a reset beyond the task's wait_limit is not waited for", tr.Status, len(tr.Attempts), tr.Error, *slept)
	}
}

func TestACanceledResetWaitCancelsTheTask(t *testing.T) {
	dir := initRepo(t)
	standIn(t)
	for b, in := range map[string]time.Duration{"codex": 20 * time.Second, "claude": time.Hour} {
		if err := limits.Note(b, agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(in)}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	orig := followup.WaitSleep
	followup.WaitSleep = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
	t.Cleanup(func() { followup.WaitSleep = orig })
	res, err := Run(ctx, Spec{Dir: dir, Backend: "codex", Options: agent.Options{WaitLimit: agent.Duration(time.Minute)},
		Tasks: []Task{{ID: "stop", Prompt: "write it"}}}, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if tr := res.Tasks[0]; tr.Status != StatusCanceled || len(tr.Attempts) != 0 {
		t.Fatalf("task = %s with %d attempts, want canceled during the wait", tr.Status, len(tr.Attempts))
	}
}

// missing is a backend whose CLI is not installed, so every attempt ends with an error at once.
type missing struct{ stub }

func (missing) Detect() (string, error) { return "", errors.New("not found on PATH") }

func TestAMissingBackendIsNotBackedOff(t *testing.T) {
	dir := initRepo(t)
	delays := recordBackoff(t)
	agent.Register(missing{stub{"wb-missing", func(context.Context, agent.Task) agent.Result {
		return agent.Result{Backend: "wb-missing", Status: agent.StatusError, Error: "wb-missing not found on PATH"}
	}}})
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-missing", Tasks: []Task{{ID: "gone", Prompt: "write it"}}})
	if tr := res.Tasks[0]; len(tr.Attempts) != 1 || len(*delays) != 0 {
		t.Fatalf("%d attempts, backoffs %v: a backend that is not installed is not retried", len(tr.Attempts), *delays)
	}
}

// noteResets remembers a reset in d for each backend.
func noteResets(t *testing.T, in map[string]time.Duration) {
	t.Helper()
	for b, d := range in {
		if err := limits.Note(b, agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(d)}); err != nil {
			t.Fatal(err)
		}
	}
}

// countClaude replaces claude with a stub that counts its runs.
func countClaude(t *testing.T) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	agent.Register(stub{"claude", func(context.Context, agent.Task) agent.Result {
		calls.Add(1)
		return agent.Result{Backend: "claude", Status: agent.StatusOK, Final: "finished", Turns: 1}
	}})
	return &calls
}

func TestARetrievalPinnedTaskWaitsOnlyForItsOwnBackend(t *testing.T) {
	opts := agent.Options{Model: "fixture", Retrieval: "required", RetrievalTenant: "t_fixture", RetrievalProject: "p_fixture",
		RetrievalRevision: strings.Repeat("a", 40), WaitLimit: agent.Duration(time.Hour)}
	for name, limitedFirst := range map[string]bool{"before the first attempt": false, "after a limited attempt": true} {
		t.Run(name, func(t *testing.T) {
			dir := initRepo(t)
			standIn(t)
			retrievalServer(t, nil)
			slept := fakeWait(t)
			claude := countClaude(t)
			var calls atomic.Int32
			agent.Register(stub{"codex", func(context.Context, agent.Task) agent.Result {
				if calls.Add(1) == 1 && limitedFirst {
					return agent.Result{Backend: "codex", Status: agent.StatusLimited, Error: "You've hit your usage limit.", LimitResets: time.Now().Add(40 * time.Minute)}
				}
				return agent.Result{Backend: "codex", Model: "fixture", Status: agent.StatusOK, Final: "finished", Turns: 1}
			}})
			noteResets(t, map[string]time.Duration{"claude": 10 * time.Minute})
			if !limitedFirst {
				noteResets(t, map[string]time.Duration{"codex": 40 * time.Minute})
			}
			res, _ := run(t, Spec{Dir: dir, Backend: "codex", Options: opts, Tasks: []Task{{ID: "pinned", Prompt: "answer"}}})
			tr := res.Tasks[0]
			if tr.Status != StatusPass || claude.Load() != 0 {
				t.Fatalf("task = %s (%q), claude ran %d times, attempts %+v; want it finished on codex alone", tr.Status, tr.Error, claude.Load(), tr.Attempts)
			}
			for _, a := range tr.Attempts {
				if a.Backend != "codex" {
					t.Fatalf("an attempt ran on %s", a.Backend)
				}
			}
			if len(*slept) != 1 || (*slept)[0] < 30*time.Minute {
				t.Fatalf("slept %v, want one wait for codex's reset, not claude's sooner one", *slept)
			}
		})
	}
}

func TestATaskWaitsWithinOneBoundAcrossItsRetryEntries(t *testing.T) {
	dir := initRepo(t)
	standIn(t)
	slept := fakeWait(t)
	var calls atomic.Int32
	agent.Register(stub{"codex", func(context.Context, agent.Task) agent.Result {
		calls.Add(1)
		return agent.Result{Backend: "codex", Status: agent.StatusLimited, Error: "You've hit your usage limit.", LimitResets: time.Now().Add(40 * time.Minute)}
	}})
	noteResets(t, map[string]time.Duration{"codex": 40 * time.Minute, "claude": 50 * time.Minute})
	res, _ := run(t, Spec{Dir: dir, Backend: "codex", Retry: []string{"codex", "codex"}, Options: agent.Options{WaitLimit: agent.Duration(time.Hour)},
		Tasks: []Task{{ID: "out", Prompt: "write it"}}})
	tr := res.Tasks[0]
	var total time.Duration
	for _, d := range *slept {
		total += d
	}
	if tr.Status != StatusFail || len(*slept) != 1 || total > time.Hour || calls.Load() != 1 {
		t.Fatalf("task = %s, slept %v (%s), %d runs; want one wait, then nothing past the hour", tr.Status, *slept, total, calls.Load())
	}
	if !strings.Contains(tr.Error+eventsText(t, res.ID), "wait limit") {
		t.Errorf("error = %q, want the spent bound said", tr.Error)
	}
}

// eventsText joins the reasons of a batch's flow, where skipped waits are explained.
func eventsText(t *testing.T, id string) string {
	t.Helper()
	var b strings.Builder
	for _, e := range mustTimeline(t, id) {
		b.WriteString(e.Reason + "\n")
	}
	return b.String()
}

func TestAnAttemptThatLeftWorkIsNotBackedOff(t *testing.T) {
	for name, act := range map[string]func(dir string) error{
		"committed": func(dir string) error {
			return exec.Command("git", "-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--allow-empty", "-qm", "half").Run()
		},
		"wrote a file": func(dir string) error { return os.WriteFile(filepath.Join(dir, "half.txt"), []byte("half\n"), 0o644) },
	} {
		t.Run(name, func(t *testing.T) {
			dir := initRepo(t)
			delays := recordBackoff(t)
			agent.Register(stub{"wb-acted", func(_ context.Context, task agent.Task) agent.Result {
				if err := act(task.Dir); err != nil {
					t.Error(err)
				}
				return agent.Result{Backend: "wb-acted", Status: agent.StatusError, Error: "connection reset"}
			}})
			res, _ := run(t, Spec{Dir: dir, Backend: "wb-acted", Retry: []string{"wb-ok"}, Tasks: []Task{{ID: "acted", Prompt: "write it"}}})
			tr := res.Tasks[0]
			if len(*delays) != 0 || len(tr.Attempts) != 2 || tr.Attempts[1].Backend != "wb-ok" {
				t.Fatalf("backoffs %v, %d attempts: an attempt that acted moves on to the retry list", *delays, len(tr.Attempts))
			}
		})
	}
}

func TestARefusalBeforeTheStartIsNotBackedOff(t *testing.T) {
	dir := initRepo(t)
	delays := recordBackoff(t)
	agent.Register(stub{"wb-refuses", func(context.Context, agent.Task) agent.Result {
		return agent.Result{Backend: "wb-refuses", Status: agent.StatusError, Error: `model "x" is not a model id`, StartRefusal: true}
	}})
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-refuses", Tasks: []Task{{ID: "refused", Prompt: "write it"}}})
	if tr := res.Tasks[0]; len(*delays) != 0 || len(tr.Attempts) != 1 {
		t.Fatalf("backoffs %v, %d attempts: a refusal repeats, so it is not retried", *delays, len(tr.Attempts))
	}
}

func TestABackoffRetryKeepsItsBackendPastRouting(t *testing.T) {
	dir := initRepo(t)
	standIn(t)
	delays := recordBackoff(t)
	claude := countClaude(t)
	var calls atomic.Int32
	orig, _ := agent.Get("wb-ok")
	agent.Register(stub{"codex", func(ctx context.Context, task agent.Task) agent.Result {
		if calls.Add(1) == 1 {
			// Another run remembers codex's limit meanwhile: routing would now start on claude.
			noteResets(t, map[string]time.Duration{"codex": time.Hour})
			return agent.Result{Backend: "codex", Status: agent.StatusError, Error: "connection reset"}
		}
		res := orig.Run(ctx, task, func(agent.Event) {})
		res.Backend = "codex"
		return res
	}})
	res, _ := run(t, Spec{Dir: dir, Backend: "codex", Tasks: []Task{{ID: "same", Prompt: "write it"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusPass || len(*delays) != 1 || claude.Load() != 0 || len(tr.Attempts) != 2 || tr.Attempts[1].Backend != "codex" {
		t.Fatalf("task = %s, backoffs %v, claude ran %d times, %d attempts; want the backoff retried on codex", tr.Status, *delays, claude.Load(), len(tr.Attempts))
	}
}

func TestAGateSafetyStopIsNotBackedOff(t *testing.T) {
	dir := initRepo(t)
	delays := recordBackoff(t)
	agent.Register(stub{"wb-ungated", func(context.Context, agent.Task) agent.Result {
		return agent.Result{Backend: "wb-ungated", Status: agent.StatusError, Error: agent.GateStopped + "a Bash call came without the gate's answer", GateStop: true}
	}})
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-ungated", Tasks: []Task{{ID: "ungated", Prompt: "write it"}}})
	if tr := res.Tasks[0]; len(*delays) != 0 || len(tr.Attempts) != 1 {
		t.Fatalf("backoffs %v, %d attempts: a run stopped for running ungated is not replayed", *delays, len(tr.Attempts))
	}
}

func TestCleanTreeIgnoresHiddenUntrackedFilesButNotChanges(t *testing.T) {
	dir := initRepo(t)
	hide := []string{"secrets/**"}
	if !cleanTree(t.Context(), dir, hide) {
		t.Fatal("a fresh repository is not clean")
	}
	if err := os.MkdirAll(filepath.Join(dir, "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secrets", "key.txt"), []byte("hidden\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !cleanTree(t.Context(), dir, hide) {
		t.Error("an untracked hidden file made the tree dirty")
	}
	if cleanTree(t.Context(), dir, nil) {
		t.Error("an untracked file outside hide counted as clean")
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cleanTree(t.Context(), dir, hide) {
		t.Error("a modified tracked file counted as clean")
	}
}
