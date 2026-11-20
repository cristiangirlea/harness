package followup

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/limits"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// fakeSleep replaces the wait's sleep for one test and records what it was asked for.
func fakeSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var slept []time.Duration
	orig := WaitSleep
	WaitSleep = func(ctx context.Context, d time.Duration) error {
		slept = append(slept, d)
		return ctx.Err()
	}
	t.Cleanup(func() { WaitSleep = orig })
	return &slept
}

func noteLimit(t *testing.T, backend, account string, until time.Time) {
	t.Helper()
	if err := limits.NoteAccount(backend, account, agent.Result{Status: agent.StatusLimited, LimitResets: until}); err != nil {
		t.Fatal(err)
	}
}

func TestWaitEndsAtTheReset(t *testing.T) {
	slept := fakeSleep(t)
	until := time.Now().Add(time.Hour)
	if err := Wait(context.Background(), until, 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	if len(*slept) != 1 || (*slept)[0] > time.Hour || (*slept)[0] < 59*time.Minute {
		t.Fatalf("slept %v, want one sleep of about an hour, up to the reset", *slept)
	}
}

func TestWaitStopsWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	start := time.Now()
	err := Wait(ctx, time.Now().Add(time.Hour), 2*time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the canceled wait took %s", took)
	}
}

func TestWaitFailsAtOnceWithoutAResetWithinTheLimit(t *testing.T) {
	slept := fakeSleep(t)
	for name, c := range map[string]struct {
		until time.Time
		max   time.Duration
		want  string
	}{
		"beyond the maximum": {time.Now().Add(3 * time.Hour), time.Hour, "further off than the wait limit of 1h0m0s"},
		"no reset":           {time.Time{}, time.Hour, "no usage-limit reset is remembered"},
		"waiting is off":     {time.Now().Add(time.Minute), 0, "wait_limit"},
	} {
		err := Wait(context.Background(), c.until, c.max)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	if len(*slept) != 0 {
		t.Fatalf("slept %v: no wait may sleep without a reset within the limit", *slept)
	}
}

func TestEarliestResetComesFromTheRememberedLimits(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	now := time.Now()
	if r := EarliestReset(now, "codex", "claude"); !r.Until.IsZero() {
		t.Fatalf("nothing is remembered, yet %+v", r)
	}
	noteLimit(t, "codex", "", now.Add(3*time.Hour))
	noteLimit(t, "claude", "", now.Add(time.Hour))
	r := EarliestReset(now, "codex", "claude")
	if r.Backend != "claude" || !r.Until.Equal(now.Add(time.Hour)) {
		t.Fatalf("earliest = %+v, want claude's reset in an hour", r)
	}
	if r := EarliestReset(now, "codex"); r.Backend != "codex" {
		t.Fatalf("earliest of codex alone = %+v", r)
	}
}

func TestWaitForWaitsForTheSoonerOfBothPlans(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t, true, true)
	slept := fakeSleep(t)
	if _, err := WaitFor(context.Background(), "codex", true, nil, time.Hour, func(Reset) { t.Error("announced a wait with no reset") }); err == nil {
		t.Fatal("waited with no remembered reset")
	}
	noteLimit(t, "codex", "", time.Now().Add(3*time.Hour))
	noteLimit(t, "claude", "", time.Now().Add(30*time.Minute))
	spent := Spent{}
	spent.Mark("claude", "")
	var heard Reset
	r, err := WaitFor(context.Background(), "codex", true, spent, time.Hour, func(r Reset) { heard = r })
	if err != nil {
		t.Fatal(err)
	}
	if r.Backend != "claude" || heard != r || len(*slept) != 1 {
		t.Fatalf("waited for %+v (announced %+v, slept %v), want claude's reset", r, heard, *slept)
	}
	if spent["claude"] {
		t.Error("claude is still marked spent after its reset")
	}
	if !strings.Contains(r.String(), "claude") {
		t.Errorf("reset text = %q", r.String())
	}
}

func TestAfterWaitsForTheResetWhenBothPlansAreOut(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t, true, true)
	slept := fakeSleep(t)
	dir := t.TempDir()
	prev := agent.Task{Prompt: "x", Dir: dir, Options: agent.Options{WaitLimit: agent.Duration(2 * time.Hour)}}
	id := limitedRun(t, prev)
	limited := agent.Result{Backend: "codex", Status: agent.StatusLimited, Error: "You've hit your usage limit.", LimitResets: time.Now().Add(time.Hour)}
	noteLimit(t, "claude", "", time.Now().Add(90*time.Minute))

	next, why, err := After(id, "codex", prev, limited)
	if err != nil || next == nil {
		t.Fatalf("no waiting carry-on: %q %v", why, err)
	}
	if next.To != "codex" || next.Until.IsZero() || !strings.Contains(next.Waiting, "codex") {
		t.Fatalf("carry-on = to %s until %v, %q; want a wait for codex", next.To, next.Until, next.Waiting)
	}
	if len(*slept) != 0 {
		t.Fatal("After slept: the caller waits")
	}
	if err := next.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*slept) != 1 || next.Plan.Backend != "codex" || !strings.Contains(next.Task.Prompt, "usage limit") {
		t.Fatalf("after the wait: slept %v, plan on %q, prompt %q", *slept, next.Plan.Backend, next.Task.Prompt)
	}
	if !strings.Contains(next.Why, "waited") {
		t.Errorf("why = %q, want it to say it waited", next.Why)
	}

	// A reset further off than the limit leaves the run limited, saying why.
	short := prev
	short.WaitLimit = agent.Duration(time.Minute)
	if next, why, err := After(id, "codex", short, limited); next != nil || err != nil || !strings.Contains(why, "wait limit") {
		t.Fatalf("beyond the limit: %v %q %v", next, why, err)
	}
	// Without the option nothing changes.
	if next, _, _ := After(id, "codex", agent.Task{Prompt: "x", Dir: dir}, limited); next != nil {
		t.Fatal("a run without wait_limit waited")
	}
	if _, err := runlog.Load(id); err != nil {
		t.Fatal(err)
	}
}

func TestAWaitNeverMovesToAPartnerThatCannotStandIn(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T){
		"bills per token": func(t *testing.T) { standIn(t, true, true); t.Setenv("ANTHROPIC_API_KEY", "fake-secret-value") },
		"not installed":   func(t *testing.T) { standIn(t, false, true) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			setup(t)
			fakeSleep(t)
			noteLimit(t, "codex", "", time.Now().Add(time.Hour))
			noteLimit(t, "claude", "", time.Now().Add(10*time.Minute))
			r, err := WaitFor(context.Background(), "codex", true, nil, 2*time.Hour, func(Reset) {})
			if err != nil || r.Backend != "codex" {
				t.Fatalf("waited for %+v (%v), want codex's own reset, not claude's sooner one", r, err)
			}
			prev := agent.Task{Prompt: "x", Dir: t.TempDir(), Options: agent.Options{WaitLimit: agent.Duration(2 * time.Hour)}}
			id := limitedRun(t, prev)
			limited := agent.Result{Backend: "codex", Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)}
			next, why, err := After(id, "codex", prev, limited)
			if err != nil || next == nil || next.To != "codex" {
				t.Fatalf("After = %+v, %q, %v; want a wait for codex", next, why, err)
			}
		})
	}
}

func TestAWaitWithoutSwitchingKeepsToItsOwnBackend(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t, true, true)
	fakeSleep(t)
	noteLimit(t, "codex", "", time.Now().Add(time.Hour))
	noteLimit(t, "claude", "", time.Now().Add(10*time.Minute))
	if r, err := WaitFor(context.Background(), "codex", false, nil, 2*time.Hour, func(Reset) {}); err != nil || r.Backend != "codex" {
		t.Fatalf("waited for %+v (%v), want codex's reset", r, err)
	}
	if r, err := WaitFor(context.Background(), "codex", true, nil, 2*time.Hour, func(Reset) {}); err != nil || r.Backend != "claude" {
		t.Fatalf("switching on: waited for %+v (%v), want claude's sooner reset", r, err)
	}
}

func TestACanceledCarryOnWaitPlansNothing(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t, true, true)
	fakeSleep(t)
	prev := agent.Task{Prompt: "x", Dir: t.TempDir(), Options: agent.Options{WaitLimit: agent.Duration(2 * time.Hour)}}
	id := limitedRun(t, prev)
	noteLimit(t, "claude", "", time.Now().Add(90*time.Minute))
	next, _, err := After(id, "codex", prev, agent.Result{Backend: "codex", Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)})
	if err != nil || next == nil {
		t.Fatal("no waiting carry-on")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := next.Wait(ctx); !errors.Is(err, context.Canceled) || next.Plan.Backend != "" {
		t.Fatalf("err = %v, plan on %q; want canceled with nothing planned", err, next.Plan.Backend)
	}
}
