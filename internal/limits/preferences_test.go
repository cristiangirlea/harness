package limits

import (
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
