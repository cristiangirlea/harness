package followup

import (
	"fmt"
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

func TestAccountChoice(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t, true, true)
	now := time.Now().Truncate(time.Second)
	a, b := prefs.Account{Name: "a", Home: t.TempDir()}, prefs.Account{Name: "b", Home: t.TempDir()}
	p := prefs.Prefs{Accounts: map[string][]prefs.Account{"codex": {a, b}}}
	save := func() {
		t.Helper()
		if err := prefs.Save(runlog.Home(), p); err != nil {
			t.Fatal(err)
		}
	}
	use := func(a prefs.Account, used int, reset time.Time) {
		t.Helper()
		dir := filepath.Join(a.Home, "sessions", "2026", "01", "01")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		line := fmt.Sprintf(`{"timestamp":%q,"payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":%d,"window_minutes":300,"resets_at":%d}}}}`, now.Format(time.RFC3339), used, reset.Unix())
		if err := os.WriteFile(filepath.Join(dir, "fixture.jsonl"), []byte(line), 0600); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}
	choice := func(asked, want string, reason bool) {
		t.Helper()
		got, configured, why := Account("codex", asked, now, nil)
		if got.Name != want || !configured || (why != "") != reason {
			t.Fatalf("Account(%q) = %q, %t, %q", asked, got.Name, configured, why)
		}
	}
	save()
	choice("", "a", false)
	use(a, 99, now.Add(time.Hour))
	choice("", "b", true)
	choice("a", "a", false)
	choice("unknown", "", true)
	use(b, 95, now.Add(time.Hour))
	choice("", "b", true)
	zero, six := 0, 6
	p.AccountReservePercent = &zero
	save()
	choice("", "a", false)
	p.AccountReservePercent = &six
	save()
	choice("", "b", true)
	use(a, 99, now.Add(-time.Minute))
	choice("", "a", false)
	limited := agent.Result{Status: agent.StatusLimited, LimitResets: now.Add(time.Hour)}
	if err := limits.Note("codex", limited); err != nil {
		t.Fatal(err)
	}
	choice("", "a", false)
	if err := limits.NoteAccount("codex", "a", limited); err != nil {
		t.Fatal(err)
	}
	choice("", "b", true)
	choice("a", "a", true)
	if err := limits.NoteAccount("codex", "b", limited); err != nil {
		t.Fatal(err)
	}
	choice("", "", true)
	if got, _, _ := Pick("codex", now, nil); got != "claude" {
		t.Fatalf("Pick = %q", got)
	}
}

func TestAccountContinuationAndCarryOn(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t, true, true)
	a, b := prefs.Account{Name: "a", Home: t.TempDir()}, prefs.Account{Name: "b", Home: t.TempDir()}
	p := prefs.Prefs{Accounts: map[string][]prefs.Account{"codex": {b, a}}}
	if err := prefs.Save(runlog.Home(), p); err != nil {
		t.Fatal(err)
	}
	agent.Register(agent.NewCodexBackend())
	task := agent.Task{Prompt: "finish the task", Dir: t.TempDir()}
	rec, err := runlog.Create("codex", task, nil)
	if err != nil {
		t.Fatal(err)
	}
	res := agent.Result{Backend: "codex", Status: agent.StatusLimited, Account: "a", LimitResets: time.Now().Add(time.Hour), Session: "saved"}
	if err := rec.Close(res); err != nil {
		t.Fatal(err)
	}
	req := Request{RunID: rec.ID(), Resume: true}
	plan, err := Make(req)
	if err != nil || plan.Account != "a" {
		t.Fatalf("pinned = %q, %v", plan.Account, err)
	}
	plan.Release()
	req.Account = "b"
	if _, err := Make(req); err == nil || !strings.Contains(err.Error(), "a continued session stays on account a") {
		t.Fatalf("wrong account: %v", err)
	}
	req.Fresh = true
	plan, err = Make(req)
	if err != nil || !plan.Fresh || plan.Account != "b" {
		t.Fatalf("fresh = %q, %v", plan.Account, err)
	}
	agent.Register(stub{"codex", true})
	next, why, err := After(rec.ID(), "codex", task, res)
	if err != nil || next == nil || next.To != "codex" || !next.Plan.Fresh || next.Plan.Account != "b" {
		t.Fatalf("carry-on = %+v, %q, %v", next, why, err)
	}
	if limits.UntilAccount("codex", "a", time.Now()).IsZero() || !limits.Until("codex", time.Now()).IsZero() {
		t.Fatal("wrong limit key")
	}
	if err := limits.NoteAccount("codex", "b", res); err != nil {
		t.Fatal(err)
	}
	next, why, err = After(rec.ID(), "codex", task, res)
	if err != nil || next == nil || next.To != "claude" {
		t.Fatalf("partner carry-on = %+v, %q, %v", next, why, err)
	}
	p.Accounts["codex"] = []prefs.Account{b}
	if err := prefs.Save(runlog.Home(), p); err != nil {
		t.Fatal(err)
	}
	if _, err := Make(Request{RunID: rec.ID(), Resume: true}); err == nil || !strings.Contains(err.Error(), "ran on account a, which is no longer declared") {
		t.Fatalf("removed account: %v", err)
	}
}

func TestBackendForConfiguresACopyAndRejectsTheRememberedAccount(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	orig, err := agent.Get("claude")
	if err != nil {
		t.Fatal(err)
	}
	registered := agent.NewClaudeBackend()
	agent.Register(registered)
	t.Cleanup(func() { agent.Register(orig) })
	home := t.TempDir()
	p := prefs.Prefs{Billing: "metered", Providers: map[string]prefs.Provider{"claude": {Target: "vertex"}},
		Accounts: map[string][]prefs.Account{"claude": {{Name: "team-a", Home: home}}}}
	if err := prefs.Save(runlog.Home(), p); err != nil {
		t.Fatal(err)
	}
	b, err := BackendFor("claude", "team-a")
	if err != nil {
		t.Fatal(err)
	}
	got := b.(*agent.CLIBackend)
	if got == registered || got.Account != "team-a" || got.AccountHome != home || got.ProviderTarget != "vertex" ||
		registered.Account != "" || registered.AccountHome != "" || registered.ProviderTarget != "" {
		t.Fatal("account and provider must be set together on the copy")
	}
	if err := limits.NoteAccount("claude", "team-a", agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := BackendFor("claude", "team-a"); err == nil || !strings.Contains(err.Error(), "at its usage limit") {
		t.Fatalf("limited account = %v", err)
	}
}

func TestChooseLimitedContinuationWithNamedAccounts(t *testing.T) {
	for _, account := range []string{"team-a", ""} {
		t.Run(account, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			standIn(t, true, true)
			p := prefs.Prefs{Accounts: map[string][]prefs.Account{"claude": {
				{Name: "team-a", Home: t.TempDir()}, {Name: "team-b", Home: t.TempDir()},
			}}}
			if err := prefs.Save(runlog.Home(), p); err != nil {
				t.Fatal(err)
			}
			rec, err := runlog.Create("claude", agent.Task{Dir: t.TempDir()}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := rec.Close(agent.Result{Backend: "claude", Account: account, Session: "saved", Status: agent.StatusOK}); err != nil {
				t.Fatal(err)
			}
			limited := agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)}
			if err := limits.NoteAccount("claude", account, limited); err != nil {
				t.Fatal(err)
			}
			pick, why := Choose("", rec.ID())
			if pick != "codex" || !strings.Contains(why, accountLabel("claude", account)+" is at its usage limit") {
				t.Errorf("Choose = %q, %q", pick, why)
			}
			plan, err := Make(Request{RunID: rec.ID(), Backend: "codex"})
			if err != nil || plan.Route() != "transcript_transfer" {
				t.Fatalf("handover: %q, %v", plan.Route(), err)
			}
			plan.Release()
			if err := limits.Note("codex", limited); err != nil {
				t.Fatal(err)
			}
			pick, why = Choose("", rec.ID())
			if pick != "" || !strings.Contains(why, "codex is at its usage limit") {
				t.Errorf("Choose without ready partner = %q, %q", pick, why)
			}
		})
	}
}
