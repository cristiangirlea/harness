package followup

import (
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/kernel"
	"github.com/cristiangirlea/harness/internal/limits"
	"github.com/cristiangirlea/harness/internal/prefs"
	"github.com/cristiangirlea/harness/internal/quota"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// fakeQuota answers plan quota from m for one test.
func fakeQuota(t *testing.T, m map[string]quota.Quota) {
	t.Helper()
	orig := planQuota
	planQuota = func(backend string, _ time.Time) quota.Quota { return m[backend] }
	t.Cleanup(func() { planQuota = orig })
}

func window(left float64, resets time.Time) *quota.Window {
	return &quota.Window{Left: left, ResetsAt: resets, ReportedAt: resets.Add(-time.Hour)}
}

func TestPickMovesANearlySpentBackendToItsPartner(t *testing.T) {
	now := time.Now()
	for name, q := range map[string]quota.Quota{
		"5h":     {FiveHour: window(3, now.Add(time.Hour)), Week: window(60, now.Add(48*time.Hour))},
		"weekly": {FiveHour: window(80, now.Add(time.Hour)), Week: window(2, now.Add(48*time.Hour))},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			standIn(t, true, true)
			fakeQuota(t, map[string]quota.Quota{"codex": q})
			pick, why, both := Pick("codex", now, nil)
			if pick != "claude" || both {
				t.Fatalf("Pick = %q (both out %v), want claude", pick, both)
			}
			for _, want := range []string{"codex has", name, "starting on claude"} {
				if !strings.Contains(why, want) {
					t.Errorf("why = %q, want it to contain %q", why, want)
				}
			}
			if pick, why := ChooseFor("codex", false); pick != "claude" || why == "" {
				t.Errorf("ChooseFor(codex, unnamed) = %q, %q; want claude and a reason", pick, why)
			}
		})
	}
}

func TestPickIgnoresUnknownOrStaleQuota(t *testing.T) {
	now := time.Now()
	for name, q := range map[string]quota.Quota{
		"unknown":       {},
		"stale":         {FiveHour: window(0, now.Add(-time.Minute)), Week: window(1, now.Add(-time.Hour))},
		"plenty":        {FiveHour: window(40, now.Add(time.Hour)), Week: window(5, now.Add(48*time.Hour))},
		"implausible":   {Week: window(0, now.Add(kernel.MaxLimitWindow+time.Hour))},
		"reset exactly": {FiveHour: window(0, now)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			standIn(t, true, true)
			fakeQuota(t, map[string]quota.Quota{"codex": q})
			if pick, why, both := Pick("codex", now, nil); pick != "codex" || why != "" || both {
				t.Errorf("Pick = %q, %q, %v; want codex, no reason", pick, why, both)
			}
		})
	}
}

func TestPickStaysOnANearlySpentBackendWhenThePartnerCannotHelp(t *testing.T) {
	now := time.Now()
	low := quota.Quota{FiveHour: window(1, now.Add(time.Hour))}

	t.Run("partner low too", func(t *testing.T) {
		t.Setenv("HRN_HOME", t.TempDir())
		standIn(t, true, true)
		fakeQuota(t, map[string]quota.Quota{"codex": low, "claude": {Week: window(4, now.Add(24*time.Hour))}})
		pick, why, both := Pick("codex", now, nil)
		if pick != "codex" || both || !strings.Contains(why, "claude has") || !strings.Contains(why, "staying on codex") {
			t.Errorf("Pick = %q, %q, %v; want codex with both quotas named", pick, why, both)
		}
	})
	t.Run("partner not installed", func(t *testing.T) {
		t.Setenv("HRN_HOME", t.TempDir())
		standIn(t, false, true)
		fakeQuota(t, map[string]quota.Quota{"codex": low})
		pick, why, both := Pick("codex", now, nil)
		if pick != "codex" || both || !strings.Contains(why, "not installed") {
			t.Errorf("Pick = %q, %q, %v; want codex and why the partner was passed over", pick, why, both)
		}
	})
	t.Run("partner limited", func(t *testing.T) {
		t.Setenv("HRN_HOME", t.TempDir())
		standIn(t, true, true)
		fakeQuota(t, map[string]quota.Quota{"codex": low})
		if err := Note("claude", agent.Result{Status: agent.StatusLimited, LimitResets: now.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		// The nearly spent backend still works, so it is not out like a limited one.
		pick, why, both := Pick("codex", now, nil)
		if pick != "codex" || both || !strings.Contains(why, "claude is at its usage limit") {
			t.Errorf("Pick = %q, %q, %v; want codex, not both out", pick, why, both)
		}
	})
}

func TestANamedBackendIsWarnedAboutNotMoved(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t, true, true)
	now := time.Now()
	fakeQuota(t, map[string]quota.Quota{"codex": {FiveHour: window(2, now.Add(time.Hour))}})
	pick, why := ChooseFor("codex", true)
	if pick != "codex" || !strings.Contains(why, "codex has") || !strings.Contains(why, "named") {
		t.Errorf("ChooseFor(codex, named) = %q, %q; want codex and a warning", pick, why)
	}
	// A remembered limit still moves a named backend: that run would fail at once.
	if err := Note("codex", agent.Result{Status: agent.StatusLimited, LimitResets: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if pick, _ := ChooseFor("codex", true); pick != "claude" {
		t.Errorf("ChooseFor(limited codex, named) = %q, want claude", pick)
	}
}

func TestAContinuationFollowsTheNamedRule(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t, true, true)
	now := time.Now()
	fakeQuota(t, map[string]quota.Quota{"codex": {FiveHour: window(2, now.Add(time.Hour))}})
	// The record ends limited but no reset is remembered, so only the quota speaks.
	id := limitedRun(t, agent.Task{Prompt: "p", Dir: t.TempDir()})
	if limits.Until("codex", now) != (time.Time{}) {
		t.Fatal("a codex limit is remembered; this test needs none")
	}

	if pick, why, err := ChooseContinuation(Request{RunID: id}, false, ""); err != nil || pick != "claude" || why == "" {
		t.Errorf("unnamed continuation = %q, %q, %v; want claude", pick, why, err)
	}
	if pick, why, err := ChooseContinuation(Request{RunID: id, Backend: "codex", Named: true}, false, ""); err != nil || pick != "codex" || !strings.Contains(why, "named") {
		t.Errorf("named continuation = %q, %q, %v; want codex and a warning", pick, why, err)
	}
	if pick, why, err := ChooseContinuation(Request{RunID: id}, true, ""); err != nil || pick != "" || why != "" {
		t.Errorf("no-switch continuation = %q, %q, %v; want it left alone", pick, why, err)
	}
}

func TestAccountsKeepTheirOwnReserve(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t, true, true)
	now := time.Now()
	p := prefs.Prefs{Accounts: map[string][]prefs.Account{"codex": {{Name: "work", Home: t.TempDir()}}}}
	if err := prefs.Save(runlog.Home(), p); err != nil {
		t.Fatal(err)
	}
	fakeQuota(t, map[string]quota.Quota{"codex": {FiveHour: window(1, now.Add(time.Hour))}})
	if pick, why, _ := Pick("codex", now, nil); pick != "codex" || why != "" {
		t.Errorf("Pick = %q, %q; a backend with accounts chooses among them instead", pick, why)
	}
}
