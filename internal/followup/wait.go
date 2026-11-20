package followup

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/limits"
	"github.com/cristiangirlea/harness/internal/prefs"
	"github.com/cristiangirlea/harness/internal/resolve"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// Reset is a remembered usage-limit reset: the backend and account it frees, and when.
type Reset struct {
	Backend, Account string
	Until            time.Time
}

func (r Reset) String() string {
	return fmt.Sprintf("%s's usage limit to reset at %s", accountLabel(r.Backend, r.Account), when(r.Until))
}

// WaitMargin is added to every wake: CLIs report resets to the minute, so the reset itself can still find the plan limited.
const WaitMargin = 30 * time.Second

// Budget bounds all of one run's or task's waits together; its deadline is taken at the first wait.
type Budget struct {
	max      time.Duration
	deadline time.Time
	asked    time.Duration
}

// NewBudget returns a budget of max for every wait of one run or task; zero never waits.
func NewBudget(max time.Duration) *Budget { return &Budget{max: max} }

// left counts the sleeps asked for as well as the clock, so a replaced sleep cannot outrun the bound.
func (b *Budget) left(now time.Time) time.Duration {
	left := b.max - b.asked
	if !b.deadline.IsZero() {
		left = min(left, b.deadline.Sub(now))
	}
	return left
}

// EarliestReset returns the soonest remembered reset among backends and their named accounts, or a zero Reset.
func EarliestReset(now time.Time, backends ...string) Reset {
	p, _ := resolve.BackendPreferences(runlog.Home())
	var best Reset
	for _, b := range backends {
		accounts := []string{""}
		if named := p.AccountsFor(b); len(named) > 0 {
			accounts = accounts[:0]
			for _, a := range named {
				accounts = append(accounts, a.Name)
			}
		}
		for _, a := range accounts {
			if until := limits.UntilAccount(b, a, now); !until.IsZero() && (best.Until.IsZero() || until.Before(best.Until)) {
				best = Reset{Backend: b, Account: a, Until: until}
			}
		}
	}
	return best
}

// soonest is the earliest reset of own, or of another backend that would be ready once it resets.
// A partner kept out by more than its limit (per-token billing, not installed) is never waited for.
func soonest(now time.Time, own string, spent Spent, others ...string) Reset {
	r := EarliestReset(now, own)
	for _, o := range others {
		c := EarliestReset(now, o)
		if c.Until.IsZero() || !r.Until.IsZero() && !c.Until.Before(r.Until) {
			continue
		}
		free := Spent{}
		for k, v := range spent {
			if k != o && !strings.HasPrefix(k, o+"@") {
				free[k] = v
			}
		}
		if ok, _ := Ready(o, c.Until.Add(WaitMargin), free); ok {
			r = c
		}
	}
	return r
}

// WaitScope is what a wait may free: any account of Backend, only Account when Pinned, and with Partner its partner too.
type WaitScope struct {
	Backend, Account string
	Pinned, Partner  bool
}

func (s WaitScope) soonest(now time.Time, spent Spent) Reset {
	if s.Pinned {
		return Reset{Backend: s.Backend, Account: s.Account, Until: limits.UntilAccount(s.Backend, s.Account, now)}
	}
	var others []string
	if other := Partner(s.Backend, now, spent); s.Partner && other != "" && other != s.Backend {
		others = append(others, other)
	}
	return soonest(now, s.Backend, spent, others...)
}

// Sleep sleeps for d or until ctx ends, returning ctx's error then.
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// WaitSleep is the sleep Wait uses; tests of every caller replace it so none sleeps for real.
var WaitSleep = Sleep

// waitable says why a wait for until would not start, or nil.
func waitable(until time.Time, b *Budget, now time.Time) error {
	switch {
	case b == nil || b.max <= 0:
		return errors.New("waiting for a usage limit is off (wait_limit is 0)")
	case until.IsZero():
		return errors.New("no usage-limit reset is remembered to wait for")
	case b.max <= WaitMargin: // no reset fits in less than the margin
		return fmt.Errorf("the wait limit of %s is too short for the %s margin past each reset", b.max, WaitMargin)
	case b.left(now) <= WaitMargin:
		return fmt.Errorf("the wait limit of %s is spent", b.max)
	case until.Add(WaitMargin).Sub(now) > b.left(now):
		if b.deadline.IsZero() {
			return fmt.Errorf("the usage limit resets at %s, further off than the wait limit of %s", when(until), b.max)
		}
		return fmt.Errorf("the usage limit resets at %s, further off than the %s left of the wait limit of %s", when(until), b.left(now).Round(time.Second), b.max)
	}
	return nil
}

// Wait sleeps until WaitMargin past the reset, within what is left of b; with no reset, or one further off, it fails at once.
func Wait(ctx context.Context, until time.Time, b *Budget) error {
	now := time.Now()
	if err := waitable(until, b, now); err != nil {
		return err
	}
	if b.deadline.IsZero() {
		b.deadline = now.Add(b.max)
	}
	d := max(until.Add(WaitMargin).Sub(now), 0)
	b.asked += d
	return WaitSleep(ctx, d)
}

// ErrChanged reports a wait that ended on a backend or account the preferences no longer allow.
var ErrChanged = errors.New("not started after the wait")

// Usable reads the preferences afresh after a wait: backend and account must still be enabled and billed to a plan.
func Usable(backend, account string) error {
	p, err := resolve.BackendPreferences(runlog.Home())
	switch {
	case err != nil:
		return fmt.Errorf("%w: the backend preferences could not be read", ErrChanged)
	case !p.Enabled(backend):
		return fmt.Errorf("%w: %s is no longer enabled", ErrChanged, backend)
	case account != "" && !slices.ContainsFunc(p.AccountsFor(backend), func(a prefs.Account) bool { return a.Name == account }):
		return fmt.Errorf("%w: account %s is no longer declared", ErrChanged, accountLabel(backend, account))
	case !p.Metered() && meteredBy(backend) != "":
		return fmt.Errorf("%w: %s would bill per token, not to a plan", ErrChanged, backend)
	}
	return nil
}

// WaitFor waits within b for the soonest reset s allows; note hears it first, and the preferences are checked again after.
// The freed account's task-local mark in spent is dropped, so the next pick can choose it.
func WaitFor(ctx context.Context, s WaitScope, spent Spent, b *Budget, note func(Reset)) (Reset, error) {
	now := time.Now()
	r := s.soonest(now, spent)
	if err := waitable(r.Until, b, now); err != nil {
		return Reset{}, err
	}
	note(r)
	if err := Wait(ctx, r.Until, b); err != nil {
		return Reset{}, err
	}
	if err := Usable(r.Backend, r.Account); err != nil {
		return Reset{}, err
	}
	delete(spent, accountLabel(r.Backend, r.Account))
	return r, nil
}

// ChooseWaiting is Choose for a new run; named says the caller typed the backend, so a nearly spent plan only warns.
// bothOut reports that the backend and its partner are both limited.
func ChooseWaiting(backend string, named bool) (pick, why string, bothOut bool) {
	meant := backend
	if meant == "" {
		meant = resolve.DefaultBackendNameIn(runlog.Home())
	}
	keep := ""
	if named {
		keep = "as named"
	}
	pick, why, bothOut = PickKeeping(meant, time.Now(), nil, keep)
	if pick == meant {
		pick = backend
	}
	return pick, why, bothOut
}
