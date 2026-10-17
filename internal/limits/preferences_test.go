package limits

import (
	"fmt"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/prefs"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestChooseAndAfterUseOnlyAnEnabledInstalledPartner(t *testing.T) {
	for _, tc := range []struct {
		name      string
		enabled   []string
		installed bool
		want      string
	}{
		{"preference order", []string{"gemini", "claude", "codex"}, true, "gemini"},
		{"skip uninstalled", []string{"gemini", "claude", "codex"}, false, "claude"},
		{"disabled partner", []string{"codex"}, true, ""},
		{"uninstalled partner", []string{"gemini", "codex"}, false, ""},
		{"no preference", nil, true, "claude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			standIn(t, true, true)
			orig, _ := agent.Get("gemini")
			agent.Register(stub{"gemini", tc.installed})
			t.Cleanup(func() { agent.Register(orig) })
			// Remember the limit before a preference can remove its partner.
			res := agent.Result{Backend: "codex", Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)}
			if err := Note("codex", res); err != nil {
				t.Fatal(err)
			}
			if tc.enabled != nil {
				if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: tc.enabled}); err != nil {
					t.Fatal(err)
				}
			}
			if got := Partner("codex"); got != tc.want {
				t.Errorf("partner %q, want %q", got, tc.want)
			}
			want := tc.want
			if want == "" {
				want = "codex"
			}
			if got, _ := Choose("codex", ""); got != want {
				t.Errorf("choice %q, want %q", got, want)
			}
			prev := agent.Task{Prompt: "hello", Dir: t.TempDir()}
			id := limitedRun(t, prev)
			next, why, err := After(id, "codex", prev, res)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if next != nil {
					t.Fatalf("unexpected carry-on: %+v", next)
				}
			} else if next == nil || next.To != tc.want {
				t.Fatalf("carry-on %+v, %s", next, why)
			}
		})
	}
}

func TestAnEnabledAPIBackendCannotBecomeABilledCarryOn(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t, true, true)
	orig, _ := agent.Get("api")
	agent.Register(stub{"api", true})
	t.Cleanup(func() { agent.Register(orig) })
	if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: []string{"codex", "api"}}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := Ready("api", time.Now(), nil); ok {
		t.Fatal("api always bills per token")
	}
	if pick, _, _ := Pick("codex", time.Now(), Spent{"codex": true}); pick != "codex" {
		t.Fatalf("switched to %s", pick)
	}
}

func TestALimitIsRememberedWhenThePreferenceHasNoPartner(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t, true, true)
	if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := Note("codex", agent.Result{Status: agent.StatusLimited, LimitResets: until}); err != nil {
		t.Fatal(err)
	}
	if got := Until("codex", time.Now()); !got.Equal(until) {
		t.Fatalf("remembered %v, want %v", got, until)
	}
}

func TestAnEnabledAPIAheadOfAPlanBackendIsSkippedAsAPartner(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t, true, true)
	orig, _ := agent.Get("api")
	agent.Register(stub{"api", true})
	t.Cleanup(func() { agent.Register(orig) })
	res := agent.Result{Backend: "codex", Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)}
	if err := Note("codex", res); err != nil {
		t.Fatal(err)
	}
	if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: []string{"codex", "api", "claude"}}); err != nil {
		t.Fatal(err)
	}
	if got := Partner("codex"); got != "claude" {
		t.Fatalf("partner %q, want claude: api bills per token and must be passed over", got)
	}
	if got, _ := Choose("codex", ""); got != "claude" {
		t.Fatalf("choice %q, want claude", got)
	}
	prev := agent.Task{Prompt: "hello", Dir: t.TempDir()}
	next, why, err := After(limitedRun(t, prev), "codex", prev, res)
	if err != nil {
		t.Fatal(err)
	}
	if next == nil || next.To != "claude" {
		t.Fatalf("carry-on %+v, %s", next, why)
	}
}

func TestAnAgyLimitWithNoPartnerEndsLimitedAndIsRemembered(t *testing.T) {
	for _, enabled := range [][]string{nil, {"agy"}, {"agy", "claude"}} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			standIn(t, false, false)
			if enabled != nil {
				if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: enabled}); err != nil {
					t.Fatal(err)
				}
			}
			until := time.Now().Add(167 * time.Hour).Truncate(time.Second)
			res := agent.Result{Backend: "agy", Status: agent.StatusLimited, LimitResets: until}
			prev := agent.Task{Prompt: "hello", Dir: t.TempDir()}
			next, why, err := After(limitedRun(t, prev), "agy", prev, res)
			if err != nil || next != nil {
				t.Fatalf("carry-on %+v, %q, %v; want none without a partner", next, why, err)
			}
			if got := Until("agy", time.Now()); !got.Equal(until) {
				t.Fatalf("remembered %v, want %v", got, until)
			}
		})
	}
}
