package followup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/limits"
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
		if ok, _ := Ready(o, c.Until.Add(time.Second), free); ok {
			r = c
		}
	}
	return r
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
func waitable(until time.Time, max time.Duration, now time.Time) error {
	switch {
	case max <= 0:
		return errors.New("waiting for a usage limit is off (wait_limit is 0)")
	case until.IsZero():
		return errors.New("no usage-limit reset is remembered to wait for")
	case until.Sub(now) > max:
		return fmt.Errorf("the usage limit resets at %s, further off than the wait limit of %s", when(until), max)
	}
	return nil
}

// Wait sleeps until the reset; with no remembered reset, or one further off than max, it fails at once.
func Wait(ctx context.Context, until time.Time, max time.Duration) error {
	now := time.Now()
	if err := waitable(until, max, now); err != nil {
		return err
	}
	return WaitSleep(ctx, until.Sub(now))
}

// WaitFor waits within max for the soonest remembered reset of backend or, with switching on, its partner; note hears it first.
// The freed account's task-local mark in spent is dropped, so the next pick can choose it.
func WaitFor(ctx context.Context, backend string, switching bool, spent Spent, max time.Duration, note func(Reset)) (Reset, error) {
	now := time.Now()
	var others []string
	if other := Partner(backend, now, spent); switching && other != "" && other != backend {
		others = append(others, other)
	}
	r := soonest(now, backend, spent, others...)
	if err := waitable(r.Until, max, now); err != nil {
		return Reset{}, err
	}
	note(r)
	if err := Wait(ctx, r.Until, max); err != nil {
		return Reset{}, err
	}
	delete(spent, accountLabel(r.Backend, r.Account))
	return r, nil
}

// ChooseWaiting is ChooseFor for a new run that also reports whether the backend and its partner are both limited.
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
