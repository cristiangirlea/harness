package followup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/limits"
	"github.com/cristiangirlea/harness/internal/prefs"
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

func TestWaitWakesAMarginPastTheReset(t *testing.T) {
	slept := fakeSleep(t)
	until := time.Now().Add(time.Hour)
	if err := Wait(context.Background(), until, NewBudget(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(*slept) != 1 || (*slept)[0] > time.Hour+WaitMargin || (*slept)[0] < time.Hour+WaitMargin-time.Minute {
		t.Fatalf("slept %v, want one sleep to %s past the reset", *slept, WaitMargin)
	}
	// The margin counts against the bound: a reset exactly at the limit cannot be waited for.
	if err := Wait(context.Background(), time.Now().Add(time.Hour), NewBudget(time.Hour)); err == nil || !strings.Contains(err.Error(), "wait limit") {
		t.Fatalf("a reset at the limit: err = %v", err)
	}
	if len(*slept) != 1 {
		t.Fatalf("slept %v", *slept)
	}
}

func TestOneBudgetBoundsEveryWaitOfARun(t *testing.T) {
	slept := fakeSleep(t)
	b := NewBudget(time.Hour)
	if err := Wait(context.Background(), time.Now().Add(40*time.Minute), b); err != nil {
		t.Fatal(err)
	}
	// About 19 minutes are left: a reset 30 minutes off no longer fits, one 10 minutes off does.
	if err := Wait(context.Background(), time.Now().Add(30*time.Minute), b); err == nil || !strings.Contains(err.Error(), "left of the wait limit of 1h0m0s") {
		t.Fatalf("second wait: err = %v, want it refused for what is left", err)
	}
	if err := Wait(context.Background(), time.Now().Add(10*time.Minute), b); err != nil {
		t.Fatal(err)
	}
	if err := Wait(context.Background(), time.Now().Add(10*time.Minute), b); err == nil {
		t.Fatal("a third wait went past the bound")
	}
	var total time.Duration
	for _, d := range *slept {
		total += d
	}
	if len(*slept) != 2 || total > time.Hour {
		t.Fatalf("slept %v (%s in all), want two waits within the hour", *slept, total)
	}
	spent := NewBudget(time.Minute)
	if err := Wait(context.Background(), time.Now().Add(29*time.Second), spent); err != nil {
		t.Fatal(err)
	}
	if err := Wait(context.Background(), time.Now().Add(time.Second), spent); err == nil || !strings.Contains(err.Error(), "spent") {
		t.Fatalf("a spent budget: err = %v", err)
	}
}

func TestWaitStopsWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	start := time.Now()
	err := Wait(ctx, time.Now().Add(time.Hour), NewBudget(2*time.Hour))
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
		"beyond the maximum":      {time.Now().Add(3 * time.Hour), time.Hour, "further off than the wait limit of 1h0m0s"},
		"no reset":                {time.Time{}, time.Hour, "no usage-limit reset is remembered"},
		"waiting is off":          {time.Now().Add(time.Minute), 0, "wait_limit"},
		"shorter than the margin": {time.Now().Add(time.Second), WaitMargin, "too short for the 30s margin"},
	} {
		err := Wait(context.Background(), c.until, NewBudget(c.max))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	if err := Wait(context.Background(), time.Now().Add(time.Minute), nil); err == nil {
		t.Error("a wait with no budget slept")
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
	if _, err := WaitFor(context.Background(), WaitScope{Backend: "codex", Partner: true}, nil, NewBudget(time.Hour), func(Reset) { t.Error("announced a wait with no reset") }); err == nil {
		t.Fatal("waited with no remembered reset")
	}
	noteLimit(t, "codex", "", time.Now().Add(3*time.Hour))
	noteLimit(t, "claude", "", time.Now().Add(30*time.Minute))
	spent := Spent{}
	spent.Mark("claude", "")
	var heard Reset
	r, err := WaitFor(context.Background(), WaitScope{Backend: "codex", Partner: true}, spent, NewBudget(time.Hour), func(r Reset) { heard = r })
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

	next, why, err := After(id, "codex", prev, limited, Waits{})
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
	if next, why, err := After(id, "codex", short, limited, Waits{}); next != nil || err != nil || !strings.Contains(why, "wait limit") {
		t.Fatalf("beyond the limit: %v %q %v", next, why, err)
	}
	// Without the option nothing changes.
	if next, _, _ := After(id, "codex", agent.Task{Prompt: "x", Dir: dir}, limited, Waits{}); next != nil {
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
			r, err := WaitFor(context.Background(), WaitScope{Backend: "codex", Partner: true}, nil, NewBudget(2*time.Hour), func(Reset) {})
			if err != nil || r.Backend != "codex" {
				t.Fatalf("waited for %+v (%v), want codex's own reset, not claude's sooner one", r, err)
			}
			prev := agent.Task{Prompt: "x", Dir: t.TempDir(), Options: agent.Options{WaitLimit: agent.Duration(2 * time.Hour)}}
			id := limitedRun(t, prev)
			limited := agent.Result{Backend: "codex", Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)}
			next, why, err := After(id, "codex", prev, limited, Waits{})
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
	if r, err := WaitFor(context.Background(), WaitScope{Backend: "codex"}, nil, NewBudget(2*time.Hour), func(Reset) {}); err != nil || r.Backend != "codex" {
		t.Fatalf("waited for %+v (%v), want codex's reset", r, err)
	}
	if r, err := WaitFor(context.Background(), WaitScope{Backend: "codex", Partner: true}, nil, NewBudget(2*time.Hour), func(Reset) {}); err != nil || r.Backend != "claude" {
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
	next, _, err := After(id, "codex", prev, agent.Result{Backend: "codex", Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)}, Waits{})
	if err != nil || next == nil {
		t.Fatal("no waiting carry-on")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := next.Wait(ctx); !errors.Is(err, context.Canceled) || next.Plan.Backend != "" {
		t.Fatalf("err = %v, plan on %q; want canceled with nothing planned", err, next.Plan.Backend)
	}
}

func TestAfterSharesTheRunsBudgetWithTheWaitBeforeItsStart(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t, true, true)
	slept := fakeSleep(t)
	prev := agent.Task{Prompt: "x", Dir: t.TempDir(), Options: agent.Options{WaitLimit: agent.Duration(2 * time.Hour)}}
	b := NewBudget(2 * time.Hour)
	if err := Wait(context.Background(), time.Now().Add(90*time.Minute), b); err != nil {
		t.Fatal(err)
	}
	id := limitedRun(t, prev)
	noteLimit(t, "claude", "", time.Now().Add(3*time.Hour))
	limited := agent.Result{Backend: "codex", Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)}
	next, why, err := After(id, "codex", prev, limited, Waits{Budget: b})
	if err != nil || next != nil || !strings.Contains(why, "left of the wait limit") {
		t.Fatalf("After = %v, %q, %v; want no second wait past what the first left", next, why, err)
	}
	if len(*slept) != 1 {
		t.Fatalf("slept %v, want only the wait before the start", *slept)
	}
}

// twoAccounts declares codex accounts a and b, with an enabled list when enabled is set.
func twoAccounts(t *testing.T, enabled ...string) prefs.Prefs {
	t.Helper()
	p := prefs.Prefs{EnabledNames: enabled, Accounts: map[string][]prefs.Account{"codex": {{Name: "a", Home: t.TempDir()}, {Name: "b", Home: t.TempDir()}}}}
	if err := prefs.Save(runlog.Home(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAWaitThatKeepsItsAccountNeverFreesAnother(t *testing.T) {
	for name, c := range map[string]struct {
		noSwitch, keep bool
	}{"no_switch": {true, false}, "an account given": {false, true}} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			standIn(t, true, true)
			twoAccounts(t)
			fakeSleep(t)
			noteLimit(t, "codex", "a", time.Now().Add(time.Hour))
			noteLimit(t, "codex", "b", time.Now().Add(10*time.Minute))
			noteLimit(t, "claude", "", time.Now().Add(5*time.Minute))
			prev := agent.Task{Prompt: "x", Dir: t.TempDir(), Options: agent.Options{NoSwitch: c.noSwitch, WaitLimit: agent.Duration(2 * time.Hour)}}
			id := limitedRun(t, prev)
			limited := agent.Result{Backend: "codex", Account: "a", Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)}
			next, why, err := After(id, "codex", prev, limited, Waits{KeepAccount: c.keep})
			if err != nil || next == nil {
				t.Fatalf("no waiting carry-on: %q %v", why, err)
			}
			if next.To != "codex" || next.req.Account != "a" || next.Until.Before(time.Now().Add(50*time.Minute)) {
				t.Fatalf("waits for %s account %q until %v; want codex@a's own reset, not b's or claude's sooner one", next.To, next.req.Account, next.Until)
			}
		})
	}
}

func TestABackendDisabledDuringTheWaitIsNotStarted(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t, true, true)
	p := twoAccounts(t, "codex", "claude")
	noteLimit(t, "codex", "a", time.Now().Add(time.Hour))
	noteLimit(t, "codex", "b", time.Now().Add(2*time.Hour))
	disable := func(context.Context, time.Duration) error {
		p.EnabledNames = []string{"claude"}
		return prefs.Save(runlog.Home(), p)
	}
	orig := WaitSleep
	WaitSleep = disable
	t.Cleanup(func() { WaitSleep = orig })
	prev := agent.Task{Prompt: "x", Dir: t.TempDir(), Options: agent.Options{NoSwitch: true, WaitLimit: agent.Duration(2 * time.Hour)}}
	id := limitedRun(t, prev)
	next, why, err := After(id, "codex", prev, agent.Result{Backend: "codex", Account: "a", Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)}, Waits{})
	if err != nil || next == nil {
		t.Fatalf("no waiting carry-on: %q %v", why, err)
	}
	if err := next.Wait(context.Background()); !errors.Is(err, ErrChanged) || next.Plan.Backend != "" {
		t.Fatalf("err = %v, plan on %q; want codex refused after the wait", err, next.Plan.Backend)
	}

	p.EnabledNames = []string{"codex", "claude"}
	if err := prefs.Save(runlog.Home(), p); err != nil {
		t.Fatal(err)
	}
	if _, err := WaitFor(context.Background(), WaitScope{Backend: "codex", Account: "a", Pinned: true}, nil, NewBudget(2*time.Hour), func(Reset) {}); !errors.Is(err, ErrChanged) {
		t.Fatalf("WaitFor err = %v, want ErrChanged", err)
	}
}

func TestAnUnsavedResetNamesNoPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	standIn(t, true, true)
	fakeSleep(t)
	// A folder where limits.json belongs makes saving fail with an error naming the path.
	if err := os.Mkdir(filepath.Join(home, "limits.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	prev := agent.Task{Prompt: "x", Dir: t.TempDir(), Options: agent.Options{NoSwitch: true, WaitLimit: agent.Duration(2 * time.Hour)}}
	id := limitedRun(t, prev)
	next, why, _ := After(id, "codex", prev, agent.Result{Backend: "codex", Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)}, Waits{})
	text := why
	if next != nil {
		text += next.Waiting + next.Why
	}
	if !strings.Contains(text, "could not be saved") || strings.Contains(text, home) || strings.Contains(text, filepath.ToSlash(home)) {
		t.Fatalf("message = %q, want the failure said without a path", text)
	}
}

func TestARetrievalPinnedRunWaitsOnlyForItsOwnBackend(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t, true, true)
	fakeSleep(t)
	noteLimit(t, "claude", "", time.Now().Add(10*time.Minute))
	for _, retrieval := range []string{"required", "off"} {
		prev := agent.Task{Prompt: "x", Dir: t.TempDir(), Options: agent.Options{Retrieval: retrieval, WaitLimit: agent.Duration(2 * time.Hour)}}
		id := limitedRun(t, prev)
		limited := agent.Result{Backend: "codex", Status: agent.StatusLimited, LimitResets: time.Now().Add(40 * time.Minute)}
		noteLimit(t, "codex", "", limited.LimitResets)
		next, why, err := After(id, "codex", prev, limited, Waits{})
		if err != nil || next == nil {
			t.Fatalf("%s: no waiting carry-on: %q %v", retrieval, why, err)
		}
		want := map[string]string{"required": "codex", "off": "claude"}[retrieval]
		if next.To != want {
			t.Fatalf("retrieval %s: waits for %s, want %s", retrieval, next.To, want)
		}
	}
}

func TestUsableRefusesAnAccountNoLongerDeclaredOrABackendThatWouldBillPerToken(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t, true, true)
	twoAccounts(t)
	if err := Usable("codex", "a"); err != nil {
		t.Fatalf("a declared account: %v", err)
	}
	if err := Usable("codex", "gone"); !errors.Is(err, ErrChanged) || !strings.Contains(err.Error(), "codex@gone is no longer declared") {
		t.Fatalf("an account no longer declared: %v", err)
	}
	t.Setenv("OPENAI_API_KEY", "fake-secret-value")
	if err := Usable("codex", "a"); !errors.Is(err, ErrChanged) || !strings.Contains(err.Error(), "bill per token") || strings.Contains(err.Error(), "fake-secret-value") {
		t.Fatalf("per-token billing: %v", err)
	}
}
