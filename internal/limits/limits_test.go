package limits

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// stub stands in for an installed CLI under its real name.
type stub struct {
	name      string
	installed bool
}

func (s stub) Name() string { return s.name }
func (s stub) Detect() (string, error) {
	if !s.installed {
		return "", errors.New("not found")
	}
	return "stub", nil
}
func (s stub) Run(context.Context, agent.Task, func(agent.Event)) agent.Result {
	return agent.Result{Backend: s.name, Status: agent.StatusOK}
}

// standIn registers stubs as claude and codex for one test, so nothing here
// can reach a real CLI, and puts the real ones back afterwards.
func standIn(t *testing.T, claudeInstalled, codexInstalled bool) {
	t.Helper()
	for _, s := range []stub{{"claude", claudeInstalled}, {"codex", codexInstalled}} {
		orig, err := agent.Get(s.name)
		if err != nil {
			t.Fatal(err)
		}
		agent.Register(s)
		t.Cleanup(func() { agent.Register(orig) })
	}
}

func TestOnlyClaudeAndCodexStandInForEachOther(t *testing.T) {
	for backend, want := range map[string]string{"claude": "codex", "codex": "claude", "api": "", "gemini": "", "opencode": "", "": ""} {
		if got := Partner(backend); got != want {
			t.Errorf("Partner(%q) = %q, want %q", backend, got, want)
		}
	}
}

func TestALimitIsRememberedUntilItLifts(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	now := time.Now()
	resets := now.Add(time.Hour).Truncate(time.Second)
	if err := Note("codex", agent.Result{Status: agent.StatusLimited, LimitResets: resets}); err != nil {
		t.Fatal(err)
	}
	if got := Until("codex", now); !got.Equal(resets) {
		t.Errorf("Until = %v, want %v", got, resets)
	}
	if got := Until("codex", now.Add(2*time.Hour)); !got.IsZero() {
		t.Errorf("after the reset Until = %v, want none", got)
	}
	if got := Until("claude", now); !got.IsZero() {
		t.Errorf("claude was never limited, Until = %v", got)
	}
}

func TestOnlyALimitWithATimeIsRemembered(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	now := time.Now()
	for _, res := range []agent.Result{
		{Status: agent.StatusLimited},
		{Status: agent.StatusError, LimitResets: now.Add(time.Hour)},
	} {
		if err := Note("codex", res); err != nil {
			t.Fatal(err)
		}
	}
	if got := Until("codex", now); !got.IsZero() {
		t.Errorf("Until = %v: a limit with no time, or a run that was not limited, is not remembered", got)
	}
}

func TestALaterNoteReplacesAnEarlierOne(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	now := time.Now()
	first, second := now.Add(5*time.Hour).Truncate(time.Second), now.Add(time.Hour).Truncate(time.Second)
	_ = Note("codex", agent.Result{Status: agent.StatusLimited, LimitResets: first})
	_ = Note("claude", agent.Result{Status: agent.StatusLimited, LimitResets: first})
	_ = Note("codex", agent.Result{Status: agent.StatusLimited, LimitResets: second})
	if got := Until("codex", now); !got.Equal(second) {
		t.Errorf("codex Until = %v, want the latest note %v", got, second)
	}
	if got := Until("claude", now); !got.Equal(first) {
		t.Errorf("claude Until = %v, want %v: one backend's note leaves the other's", got, first)
	}
}

func TestAnUnreadableFileIsNoLimit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	if err := writeFile(home+"/limits.json", []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	if got := Until("codex", time.Now()); !got.IsZero() {
		t.Errorf("Until = %v from a broken file", got)
	}
	// Noting starts the file over rather than failing every run.
	if err := Note("codex", agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if Until("codex", time.Now()).IsZero() {
		t.Error("the note after a broken file was not kept")
	}
}

func TestPickMovesALimitedBackendToItsPartner(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t, true, true)
	now := time.Now()
	_ = Note("codex", agent.Result{Status: agent.StatusLimited, LimitResets: now.Add(time.Hour)})

	pick, why, _ := Pick("codex", now, nil)
	if pick != "claude" {
		t.Fatalf("Pick = %q, want claude", pick)
	}
	if !strings.Contains(why, "codex") || !strings.Contains(why, "claude") || !strings.Contains(why, "usage limit") {
		t.Errorf("why = %q, want it to name both and the limit", why)
	}
	if pick, why, _ := Pick("claude", now, nil); pick != "claude" || why != "" {
		t.Errorf("an unlimited backend stays: %q %q", pick, why)
	}
	if pick, _, _ := Pick("api", now, nil); pick != "api" {
		t.Errorf("api has no partner and stays: %q", pick)
	}
}

func TestPickStaysWhenThePartnerCannotTakeOver(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	now := time.Now()
	_ = Note("codex", agent.Result{Status: agent.StatusLimited, LimitResets: now.Add(time.Hour)})

	standIn(t, false, true)
	if pick, _, both := Pick("codex", now, nil); pick != "codex" || both {
		t.Errorf("claude is not installed, Pick = %q (both out %v)", pick, both)
	}

	standIn(t, true, true)
	_ = Note("claude", agent.Result{Status: agent.StatusLimited, LimitResets: now.Add(2 * time.Hour)})
	pick, why, both := Pick("codex", now, nil)
	if pick != "codex" || !both {
		t.Errorf("claude is limited too, Pick = %q (both out %v)", pick, both)
	}
	if !strings.Contains(why, "claude is at its usage limit") || !strings.Contains(why, "codex is at its usage limit") {
		t.Errorf("why = %q, want both named", why)
	}
}

func TestPickCountsALimitReachedInTheCallersOwnWork(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t, true, true)
	now := time.Now()
	// The Fable limit gives no time, so nothing is on file; the task knows.
	pick, why, _ := Pick("claude", now, Spent{"claude": true})
	if pick != "codex" || !strings.Contains(why, "earlier in this task") {
		t.Errorf("Pick = %q, %q", pick, why)
	}
	if _, _, both := Pick("claude", now, Spent{"claude": true, "codex": true}); !both {
		t.Error("both spent in this task, yet not both out")
	}
}

// limitedRun records a finished codex run that reached its limit.
func limitedRun(t *testing.T, task agent.Task) string {
	t.Helper()
	rec, err := runlog.Create("codex", task)
	if err != nil {
		t.Fatal(err)
	}
	res := agent.Result{Backend: "codex", Status: agent.StatusLimited, Error: "You've hit your usage limit.",
		Final: "Half of the fix is in.", LimitResets: time.Now().Add(time.Hour)}
	if err := rec.Close(res); err != nil {
		t.Fatal(err)
	}
	return rec.ID()
}

func TestAfterCarriesALimitedRunOnToItsPartner(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t, true, true)
	approved := false
	prev := agent.Task{Prompt: "fix the bug", Dir: t.TempDir(),
		Options: agent.Options{Mode: agent.ModeYolo, Model: "gpt-5.5", Timeout: agent.Duration(time.Minute)},
		Approve: func(agent.ApprovalRequest) bool { approved = true; return true }}
	id := limitedRun(t, prev)
	res := agent.Result{Backend: "codex", Status: agent.StatusLimited, Error: "You've hit your usage limit.", LimitResets: time.Now().Add(time.Hour)}

	next, why, err := After(id, "codex", prev, res)
	if err != nil {
		t.Fatal(err)
	}
	if next == nil {
		t.Fatalf("no carry-on: %s", why)
	}
	if next.To != "claude" || next.From != "codex" {
		t.Errorf("from %s to %s, want codex to claude", next.From, next.To)
	}
	if next.Task.Mode != agent.ModeYolo || next.Task.Timeout != prev.Timeout {
		t.Errorf("options not carried over: %+v", next.Task.Options)
	}
	if next.Task.Model != "" {
		t.Errorf("model = %q: codex's model means nothing to claude", next.Task.Model)
	}
	if !strings.Contains(next.Task.Prompt, "usage limit") || !strings.Contains(next.Task.Prompt, "codex") {
		t.Errorf("prompt = %q, want it to say why the turn stopped", next.Task.Prompt)
	}
	if len(next.Plan.Handoff) != 1 || next.Plan.Handoff[0].ID != id {
		t.Errorf("handoff = %v, want the limited run", next.Plan.Handoff)
	}
	if next.Task.Approve == nil {
		t.Fatal("the approver is not carried over")
	}
	next.Task.Approve(agent.ApprovalRequest{})
	if !approved {
		t.Error("the carried approver is not the original")
	}
	if !strings.Contains(next.Why, "codex") || !strings.Contains(next.Why, "claude") {
		t.Errorf("why = %q", next.Why)
	}
	// The limit is remembered, so the next run goes straight to claude.
	if Until("codex", time.Now()).IsZero() {
		t.Error("After did not remember codex's limit")
	}
}

func TestAfterLeavesARunAloneWhenItShould(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t, true, true)
	dir := t.TempDir()
	limited := agent.Result{Backend: "codex", Status: agent.StatusLimited, Error: "You've hit your usage limit."}

	id := limitedRun(t, agent.Task{Prompt: "x", Dir: dir})
	if next, why, err := After(id, "codex", agent.Task{Prompt: "x", Dir: dir}, agent.Result{Status: agent.StatusError}); next != nil || why != "" || err != nil {
		t.Errorf("a run that was not limited: %v %q %v", next, why, err)
	}
	off := agent.Task{Prompt: "x", Dir: dir, Options: agent.Options{NoSwitch: true}}
	if next, why, err := After(id, "codex", off, limited); next != nil || why != "" || err != nil {
		t.Errorf("no_switch: %v %q %v", next, why, err)
	}
	if next, _, _ := After(id, "api", agent.Task{Prompt: "x", Dir: dir}, limited); next != nil {
		t.Error("api has no partner")
	}

	_ = Note("claude", agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)})
	next, why, err := After(id, "codex", agent.Task{Prompt: "x", Dir: dir}, limited)
	if next != nil || err != nil {
		t.Fatalf("claude is limited too, yet: %v %v", next, err)
	}
	if !strings.Contains(why, "claude") {
		t.Errorf("why = %q, want it to say claude is limited too", why)
	}
}

func TestStatusSaysWhatIsRememberedAndHowToForgetIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	now := time.Now()
	if s := Status("codex", now); s != "" {
		t.Errorf("nothing remembered, Status = %q", s)
	}
	_ = Note("codex", agent.Result{Status: agent.StatusLimited, LimitResets: now.Add(time.Hour)})
	s := Status("codex", now)
	if !strings.Contains(s, "usage limit until") || !strings.Contains(s, "limits.json") {
		t.Errorf("Status = %q, want the time and the file to delete", s)
	}
}
