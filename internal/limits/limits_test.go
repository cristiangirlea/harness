package limits

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/retrieval"
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
	// A key in the environment would keep the partner from standing in.
	for _, name := range append(MeteredEnv("claude"), MeteredEnv("codex")...) {
		t.Setenv(name, "")
	}
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
		if got := Partner(backend, time.Now(), nil); got != want {
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

func TestAfterRefusesToRepinRetrievalOnThePartner(t *testing.T) {
	for _, mode := range []string{"required", "off"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			standIn(t, true, true)
			prev := agent.Task{Prompt: "fix the bug", Dir: t.TempDir(), Options: agent.Options{Retrieval: mode}}
			rec, err := runlog.Create("codex", prev)
			if err != nil {
				t.Fatal(err)
			}
			res := agent.Result{Backend: "codex", Status: agent.StatusLimited, Error: "You've hit your usage limit.",
				Retrieval: &retrieval.Record{Mode: "required", Packet: &retrieval.Packet{VersionID: "kv-7"}}}
			if err := rec.Close(res); err != nil {
				t.Fatal(err)
			}
			next, why, err := After(rec.ID(), "codex", prev, res)
			if mode == "off" {
				if err != nil || next == nil {
					t.Fatalf("retrieval off should still carry on: %v %s", err, why)
				}
				return
			}
			if next != nil || err == nil || !strings.Contains(err.Error(), "kv-7") {
				t.Fatalf("claude would repin silently: next = %+v, err = %v", next, err)
			}
		})
	}
}

func TestACarryOnAfterAFreshHandoverDoesNotReachPastIt(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t, true, true)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old, err := runlog.Create("claude", agent.Task{Prompt: "one", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Close(agent.Result{Backend: "claude", Status: agent.StatusOK, Session: "s1", Final: "a1"}); err != nil {
		t.Fatal(err)
	}
	prev := agent.Task{Prompt: "two", Dir: dir}
	rec, err := runlog.CreateContinuingRoute("codex", prev, old.ID(), runlog.RouteFreshHandover)
	if err != nil {
		t.Fatal(err)
	}
	res := agent.Result{Backend: "codex", Status: agent.StatusLimited, Error: "You've hit your usage limit.", LimitResets: time.Now().Add(time.Hour)}
	if err := rec.Close(res); err != nil {
		t.Fatal(err)
	}
	next, why, err := After(rec.ID(), "codex", prev, res)
	if err != nil || next == nil {
		t.Fatalf("no carry-on: %s err=%v", why, err)
	}
	if next.Plan.ForkSession != "" || len(next.Plan.Handoff) != 1 || next.Plan.Handoff[0].ID != rec.ID() {
		t.Errorf("claude must not fork the session the fresh run left behind: %+v", next.Plan)
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

func TestNotesFromParallelWorkersAreAllKept(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	now := time.Now()
	// A batch's workers note and read limits at once; on Windows a read can
	// also hold the file a rename wants to replace.
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(2)
		name := []string{"codex", "claude"}[i%2]
		go func() {
			defer wg.Done()
			if err := Note(name, agent.Result{Status: agent.StatusLimited, LimitResets: now.Add(time.Hour)}); err != nil {
				t.Errorf("note: %v", err)
			}
		}()
		go func() { defer wg.Done(); Until(name, now) }()
	}
	wg.Wait()
	for _, name := range []string{"codex", "claude"} {
		if Until(name, now).IsZero() {
			t.Errorf("%s's note was lost", name)
		}
	}
}

func TestANullFileIsNoLimit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	if err := writeFile(home+"/limits.json", []byte("null")); err != nil {
		t.Fatal(err)
	}
	if err := Note("codex", agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if Until("codex", time.Now()).IsZero() {
		t.Error("the note after a null file was not kept")
	}
}

// TestHelperNote is not a test: the test binary, run as a second hrn process,
// notes a limit over and over.
func TestHelperNote(t *testing.T) {
	name := os.Getenv("HRN_LIMITS_HELPER")
	if name == "" {
		return
	}
	for i := 0; i < 30; i++ {
		if err := Note(name, agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
	}
	os.Exit(0)
}

func TestNotesFromOtherProcessesDoNotFail(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	var cmds []*exec.Cmd
	for _, name := range []string{"codex", "claude"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperNote$")
		cmd.Env = append(os.Environ(), "HRN_LIMITS_HELPER="+name)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
	}
	// Reading meanwhile is what makes Windows refuse a rename.
	for i := 0; i < 100; i++ {
		Until("codex", time.Now())
	}
	for _, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Errorf("a process noting a limit failed: %v", err)
		}
	}
}

// A partner whose CLI would bill per token is no stand-in: nobody chose it, and
// the switch is for the plans. Only the variable's name is ever shown.
func TestAPartnerThatWouldBillPerTokenDoesNotStandIn(t *testing.T) {
	for _, c := range []struct{ limited, partner, key string }{
		{"codex", "claude", "ANTHROPIC_API_KEY"},
		{"codex", "claude", "CLAUDE_CODE_USE_BEDROCK"},
		{"claude", "codex", "CODEX_API_KEY"},
	} {
		t.Run(c.key, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			standIn(t, true, true)
			t.Setenv(c.key, "fake-secret-value")
			now := time.Now()

			ok, why := Ready(c.partner, now, nil)
			if ok || !strings.Contains(why, c.key) || !strings.Contains(why, "per token") {
				t.Fatalf("Ready(%s) = %v, %q; want refused, naming %s", c.partner, ok, why, c.key)
			}
			if strings.Contains(why, "fake-secret-value") {
				t.Fatalf("the reason shows the variable's value: %q", why)
			}

			_ = Note(c.limited, agent.Result{Status: agent.StatusLimited, LimitResets: now.Add(time.Hour)})
			if pick, _, both := Pick(c.limited, now, nil); pick != c.limited || both {
				t.Errorf("Pick(%s) = %q (both out %v); want it to stay", c.limited, pick, both)
			}

			prev := agent.Task{Prompt: "p", Dir: t.TempDir()}
			rec, err := runlog.Create(c.limited, prev)
			if err != nil {
				t.Fatal(err)
			}
			res := agent.Result{Backend: c.limited, Status: agent.StatusLimited, LimitResets: now.Add(time.Hour)}
			if err := rec.Close(res); err != nil {
				t.Fatal(err)
			}
			next, why, err := After(rec.ID(), c.limited, prev, res)
			if err != nil || next != nil || !strings.Contains(why, "per token") {
				t.Errorf("After = %v, %q, %v; want no carry-on and the reason", next, why, err)
			}
		})
	}
	// Set to 0 or false, the switch is off, and so is the refusal.
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t, true, true)
	t.Setenv("CLAUDE_CODE_USE_VERTEX", "0")
	if ok, why := Ready("claude", time.Now(), nil); !ok {
		t.Errorf("CLAUDE_CODE_USE_VERTEX=0 refused claude: %s", why)
	}
}

func TestMeteredKeysTreatEveryNonEmptyValueAsSet(t *testing.T) {
	for backend, names := range metered {
		for _, name := range names {
			for _, value := range []string{"", "0", "false", "FALSE", "1", "fake-secret-value"} {
				t.Run(name+"/"+value, func(t *testing.T) {
					t.Setenv("HRN_HOME", t.TempDir())
					standIn(t, true, true)
					t.Setenv(name, value)
					want := value != ""
					if strings.HasPrefix(name, "CLAUDE_CODE_USE_") && (value == "0" || strings.EqualFold(value, "false")) {
						want = false
					}
					got := meteredBy(backend)
					if want && got != name || !want && got != "" {
						t.Errorf("meteredBy(%q) = %q, want set=%v for %s", backend, got, want, name)
					}
				})
			}
		}
	}
}

func TestPickAndChooseExplainWhyAMeteredPartnerWasSkipped(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	standIn(t, true, true)
	t.Setenv("ANTHROPIC_API_KEY", "fake-secret-value")
	now := time.Now()
	if err := Note("codex", agent.Result{Status: agent.StatusLimited, LimitResets: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	pick, why, both := Pick("codex", now, nil)
	if pick != "codex" || both || !strings.Contains(why, "ANTHROPIC_API_KEY") || !strings.Contains(why, "per token") {
		t.Errorf("Pick = %q, %q, %v; want codex and the refusal reason", pick, why, both)
	}
	for _, backend := range []string{"codex", ""} {
		// An implicit backend comes from the continued record, without changing
		// the caller's empty selection when the partner cannot take over.
		id := limitedRun(t, agent.Task{Prompt: "p", Dir: t.TempDir()})
		chosen, reason := Choose(backend, id)
		if chosen != backend || !strings.Contains(reason, "ANTHROPIC_API_KEY") || !strings.Contains(reason, "per token") {
			t.Errorf("Choose = %q, %q; want %q and the refusal reason", chosen, reason, backend)
		}
		if strings.Contains(reason, "fake-secret-value") {
			t.Error("Choose exposed the key value")
		}
	}
}

// A time hrn would not have believed from a CLI is not believed from the file.
func TestARememberedLimitFurtherAheadThanACLICouldSayIsIgnored(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	now := time.Now()
	data := fmt.Sprintf(`{"codex":{"until":%q},"claude":{"until":%q}}`,
		now.Add(agent.MaxLimitWindow+time.Hour).Format(time.RFC3339), now.Add(time.Hour).Format(time.RFC3339))
	if err := writeFile(home+"/limits.json", []byte(data)); err != nil {
		t.Fatal(err)
	}
	if got := Until("codex", now); !got.IsZero() {
		t.Errorf("codex Until = %v, from a time beyond %v ahead", got, agent.MaxLimitWindow)
	}
	if Until("claude", now).IsZero() {
		t.Error("claude's ordinary limit was dropped with it")
	}
}
