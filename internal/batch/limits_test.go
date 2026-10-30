package batch

import (
	"context"
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

// standIn puts stubs in place of claude and codex for one test, so no real CLI
// can run. codex writes half the work and reaches its limit; claude writes the
// rest and finishes.
func standIn(t *testing.T) {
	t.Helper()
	// A key in the environment would keep the partner from standing in.
	for _, name := range append(limits.MeteredEnv("claude"), limits.MeteredEnv("codex")...) {
		t.Setenv(name, "")
	}
	for _, s := range []stub{
		{"codex", func(_ context.Context, task agent.Task) agent.Result {
			_ = os.WriteFile(filepath.Join(task.Dir, "half.txt"), []byte("first half\n"), 0o644)
			return agent.Result{Backend: "codex", Status: agent.StatusLimited, Error: "You've hit your usage limit.",
				Final: "Wrote the first half.", LimitResets: time.Now().Add(time.Hour)}
		}},
		{"claude", func(_ context.Context, task agent.Task) agent.Result {
			_ = os.WriteFile(filepath.Join(task.Dir, "done.txt"), []byte("second half\n"), 0o644)
			return agent.Result{Backend: "claude", Status: agent.StatusOK, Final: "finished", Turns: 1}
		}},
	} {
		orig, err := agent.Get(s.name)
		if err != nil {
			t.Fatal(err)
		}
		agent.Register(s)
		t.Cleanup(func() { agent.Register(orig) })
	}
}

func TestALimitedAttemptIsCarriedOnInTheSameWorktree(t *testing.T) {
	dir := initRepo(t)
	standIn(t)
	res, events := run(t, Spec{Dir: dir, Backend: "codex", Options: agent.Options{Model: "gpt-5.5"},
		Gate:  []string{"test -f half.txt && test -f done.txt"},
		Tasks: []Task{{ID: "halves", Prompt: "write both halves"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusPass || len(tr.Attempts) != 2 {
		t.Fatalf("task = %+v", tr)
	}
	a1, a2 := tr.Attempts[0], tr.Attempts[1]
	if a1.Backend != "codex" || a1.Result.Status != agent.StatusLimited || len(a1.Gates) != 0 {
		t.Errorf("attempt 1: %s %s, %d gates", a1.Backend, a1.Result.Status, len(a1.Gates))
	}
	if a2.Backend != "claude" || !a2.Continued || a2.SwitchedFrom != "codex" {
		t.Errorf("attempt 2: %s, continued %v, switched from %q", a2.Backend, a2.Continued, a2.SwitchedFrom)
	}
	for _, want := range []string{"write both halves", "attempt 1 on codex", "usage limit", "committed on the current branch", "Wrote the first half."} {
		if !strings.Contains(a2.Prompt, want) {
			t.Errorf("attempt 2 prompt is missing %q:\n%s", want, a2.Prompt)
		}
	}
	rec, err := runlog.Load(a2.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Continues != a1.RunID {
		t.Errorf("run %s continues %q, want the limited run %s", a2.RunID, rec.Continues, a1.RunID)
	}
	if rec.Task.Model != "" {
		t.Errorf("model = %q: codex's model means nothing to claude", rec.Task.Model)
	}
	if n := git(t, dir, "rev-list", "--count", res.Base+"..hrn/halves"); n != "2" {
		t.Errorf("commits above base = %s, want both attempts' work", n)
	}
	if retries := eventTexts(events, "retry"); !strings.Contains(retries, "claude") || !strings.Contains(retries, "usage limit") {
		t.Errorf("retry events = %q, want the switch named", retries)
	}
	if limits.Until("codex", time.Now()).IsZero() {
		t.Error("codex's limit was not remembered")
	}
}

func TestALeanClaudeAttemptCarriesOnToCodexWithoutLean(t *testing.T) {
	dir := initRepo(t)
	standIn(t)
	agent.Register(leanStub{stub{"claude", func(context.Context, agent.Task) agent.Result {
		return agent.Result{Backend: "claude", Status: agent.StatusLimited, Error: "You've hit your usage limit."}
	}}})
	agent.Register(stub{"codex", func(context.Context, agent.Task) agent.Result {
		return agent.Result{Backend: "codex", Status: agent.StatusOK, Final: "finished", Turns: 1}
	}})
	res, _ := run(t, Spec{Dir: dir, Backend: "claude", Tasks: []Task{{Prompt: "finish the work"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusPass || len(tr.Attempts) != 2 {
		t.Fatalf("task = %+v, want a limited claude attempt followed by codex", tr)
	}
	a1, a2 := tr.Attempts[0], tr.Attempts[1]
	if a1.Backend != "claude" || a1.Result.Status != agent.StatusLimited {
		t.Fatalf("first attempt = %+v, want limited claude", a1)
	}
	if rec := firstRun(t, tr); !rec.Task.IsLean() {
		t.Fatal("the claude attempt must record the batch's lean default")
	}
	if a2.Backend != "codex" || !a2.Continued || a2.SwitchedFrom != "claude" {
		t.Fatalf("second attempt = %+v, want a carry-on on codex", a2)
	}
	rec, err := runlog.Load(a2.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Continues != a1.RunID || rec.Result.Backend != "codex" {
		t.Errorf("codex record must continue the limited claude run: %+v", rec)
	}
	if rec.Task.Lean != nil {
		t.Errorf("codex record must leave lean unset, got %v", rec.Task.Lean)
	}
}

func TestACarryOnDoesNotUseUpARetry(t *testing.T) {
	dir := initRepo(t)
	standIn(t)
	// claude's half does not pass the gate; wb-ok, the one retry, writes it.
	res, _ := run(t, Spec{Dir: dir, Backend: "codex", Retry: []string{"wb-ok"},
		Gate:  []string{"test -f out.txt"},
		Tasks: []Task{{ID: "three", Prompt: "p"}}})
	tr := res.Tasks[0]
	var got []string
	for _, a := range tr.Attempts {
		got = append(got, a.Backend)
	}
	if tr.Status != StatusPass || strings.Join(got, ",") != "codex,claude,wb-ok" {
		t.Fatalf("status %s, attempts %v, want codex, its carry-on on claude, then the retry", tr.Status, got)
	}
}

func TestAnAttemptMeantForALimitedBackendStartsOnItsPartner(t *testing.T) {
	dir := initRepo(t)
	standIn(t)
	_ = limits.Note("codex", agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)})
	res, events := run(t, Spec{Dir: dir, Backend: "codex", Options: agent.Options{Model: "gpt-5.5"},
		Tasks: []Task{{ID: "moved", Prompt: "p"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusPass || len(tr.Attempts) != 1 {
		t.Fatalf("task = %+v", tr)
	}
	if a := tr.Attempts[0]; a.Backend != "claude" || a.SwitchedFrom != "codex" {
		t.Errorf("attempt on %s switched from %q", a.Backend, a.SwitchedFrom)
	}
	if rec := firstRun(t, tr); rec.Task.Model != "" {
		t.Errorf("model = %q, want none for claude", rec.Task.Model)
	}
	if s := eventTexts(events, "switch"); !strings.Contains(s, "codex is at its usage limit") {
		t.Errorf("switch events = %q", s)
	}
}

func TestARoutedAttemptMovedByALimitDropsTheRoutedChoice(t *testing.T) {
	dir := initRepo(t)
	standIn(t)
	_ = limits.Note("codex", agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)})
	if err := os.WriteFile(filepath.Join(runlog.Home(), "route.json"), []byte(`{"implement":{"backend":"codex","model":"routed","effort":"medium","eval":"e1"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	res, _ := run(t, Spec{Dir: dir, Tasks: []Task{{ID: "moved", Prompt: "Implement a widget"}}})
	tr := res.Tasks[0]
	if len(tr.Attempts) != 1 || tr.Attempts[0].Backend != "claude" || tr.Attempts[0].SwitchedFrom != "codex" {
		t.Fatalf("attempts = %+v, want one moved from codex to claude", tr.Attempts)
	}
	if rec := firstRun(t, tr); rec.Task.Model != "" || rec.Task.Effort != "" || rec.RoutedBy != nil {
		t.Errorf("moved attempt kept model %q, effort %q, routed by %+v", rec.Task.Model, rec.Task.Effort, rec.RoutedBy)
	}
}

func TestATaskStopsWhenBothAreLimited(t *testing.T) {
	dir := initRepo(t)
	standIn(t)
	for _, name := range []string{"codex", "claude"} {
		_ = limits.Note(name, agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)})
	}
	res, _ := run(t, Spec{Dir: dir, Backend: "codex", Tasks: []Task{{ID: "none", Prompt: "p"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusFail || len(tr.Attempts) != 0 {
		t.Fatalf("task = %+v, want it stopped before any agent ran", tr)
	}
	if !strings.Contains(tr.Error, "claude") || !strings.Contains(tr.Error, "codex") {
		t.Errorf("error = %q, want both named", tr.Error)
	}
}

func TestNoSwitchKeepsABatchOnItsBackend(t *testing.T) {
	dir := initRepo(t)
	standIn(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "codex", Options: agent.Options{NoSwitch: true},
		Tasks: []Task{{ID: "stays", Prompt: "p"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusFail || len(tr.Attempts) != 1 || tr.Attempts[0].Backend != "codex" {
		t.Fatalf("task = %+v, want one limited attempt on codex", tr)
	}
}

func TestARetryMeantForALimitedBackendRunsOnItsPartner(t *testing.T) {
	dir := initRepo(t)
	standIn(t)
	_ = limits.Note("codex", agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)})
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-bad", Retry: []string{"codex"},
		Tasks: []Task{{ID: "retried", Prompt: "p"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusPass || len(tr.Attempts) != 2 {
		t.Fatalf("task = %+v", tr)
	}
	if a := tr.Attempts[1]; a.Backend != "claude" || a.SwitchedFrom != "codex" {
		t.Errorf("the retry ran on %s, switched from %q", a.Backend, a.SwitchedFrom)
	}
}

func TestWhenBothRunOutInATaskNothingStartsAThirdTime(t *testing.T) {
	dir := initRepo(t)
	standIn(t)
	orig, _ := agent.Get("claude")
	// The Fable wording gives no time, so only the task itself knows.
	agent.Register(stub{"claude", func(context.Context, agent.Task) agent.Result {
		return agent.Result{Backend: "claude", Status: agent.StatusLimited, Error: "You've reached your Fable limit."}
	}})
	t.Cleanup(func() { agent.Register(orig) })

	res, _ := run(t, Spec{Dir: dir, Backend: "codex", Retry: []string{"codex", "claude"},
		Tasks: []Task{{ID: "spent", Prompt: "p"}}})
	tr := res.Tasks[0]
	var got []string
	for _, a := range tr.Attempts {
		got = append(got, a.Backend)
	}
	if tr.Status != StatusFail || strings.Join(got, ",") != "codex,claude" {
		t.Fatalf("status %s, attempts %v: after both ran out, the retries are skipped", tr.Status, got)
	}
}

// With claude set to bill per token, a limited attempt on codex is not carried
// on there, and the batch says why.
func TestALimitedAttemptIsNotCarriedOnToAPartnerThatBillsPerToken(t *testing.T) {
	dir := initRepo(t)
	standIn(t)
	t.Setenv("ANTHROPIC_API_KEY", "fake-secret-value")
	res, events := run(t, Spec{Dir: dir, Backend: "codex", Tasks: []Task{{ID: "kept", Prompt: "p"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusFail || len(tr.Attempts) != 1 || tr.Attempts[0].Backend != "codex" {
		t.Fatalf("task = %+v, want one limited attempt on codex and nothing on claude", tr)
	}
	switches := eventTexts(events, "switch")
	if !strings.Contains(switches, "not carried on") || !strings.Contains(switches, "ANTHROPIC_API_KEY") {
		t.Errorf("switch events = %q, want the reason", switches)
	}
	if strings.Contains(switches, "fake-secret-value") {
		t.Errorf("an event shows the key's value: %q", switches)
	}
}

func TestABatchExplainsAMeteredPartnerBeforeTheAttemptStarts(t *testing.T) {
	dir := initRepo(t)
	standIn(t)
	t.Setenv("ANTHROPIC_API_KEY", "fake-secret-value")
	if err := limits.Note("codex", agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	_, events := run(t, Spec{Dir: dir, Backend: "codex", Tasks: []Task{{ID: "kept", Prompt: "p"}}})
	said := false
	for _, e := range events {
		if e.Kind == "start" {
			if !said {
				t.Fatal("no metered partner reason before the attempt started")
			}
			return
		}
		if e.Kind == "switch" {
			t.Errorf("unexpected switch event when nothing switched: %+v", e)
		}
		said = said || e.Kind == "info" && strings.Contains(e.Text, "ANTHROPIC_API_KEY") && strings.Contains(e.Text, "per token")
		if strings.Contains(e.Text, "fake-secret-value") {
			t.Error("event exposed the key value")
		}
	}
	t.Fatal("no attempt started")
}

func TestABatchDoesNotCreateACarryOnAfterCancellation(t *testing.T) {
	dir := initRepo(t)
	standIn(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agent.Register(stub{"codex", func(context.Context, agent.Task) agent.Result {
		cancel()
		return agent.Result{Backend: "codex", Status: agent.StatusLimited, Error: "usage limit"}
	}})
	res, err := Run(ctx, Spec{Dir: dir, Backend: "codex", Tasks: []Task{{ID: "stopped", Prompt: "p"}}}, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tasks) != 1 || res.Tasks[0].Status != StatusCanceled || len(res.Tasks[0].Attempts) != 1 {
		t.Fatalf("tasks = %+v, want one canceled task with one attempt", res.Tasks)
	}
	recs, err := runlog.List(5)
	if err != nil || len(recs) != 1 || recs[0].Backend != "codex" {
		t.Fatalf("records = %+v, err=%v; want only the limited run", recs, err)
	}
}

// preferThird enables codex, claude and agy (plus extra) with an agy stub
// that finishes, and claude limited as well as codex.
func preferThird(t *testing.T, extra ...string) {
	t.Helper()
	t.Setenv("HRN_HOME", t.TempDir())
	if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: append([]string{"codex", "claude", "agy"}, extra...)}); err != nil {
		t.Fatal(err)
	}
	standIn(t)
	for _, s := range []stub{
		{"claude", func(context.Context, agent.Task) agent.Result {
			return agent.Result{Backend: "claude", Status: agent.StatusLimited, Error: "You've reached your Fable limit."}
		}},
		{"agy", func(_ context.Context, task agent.Task) agent.Result {
			_ = os.WriteFile(filepath.Join(task.Dir, "done.txt"), []byte("second half\n"), 0o644)
			return agent.Result{Backend: "agy", Status: agent.StatusOK, Final: "finished", Turns: 1}
		}},
	} {
		orig, err := agent.Get(s.name)
		if err != nil {
			t.Fatal(err)
		}
		agent.Register(s)
		t.Cleanup(func() { agent.Register(orig) })
	}
}

func TestWithAPreferenceACarryOnGoesPastTwoSpentBackends(t *testing.T) {
	dir := initRepo(t)
	preferThird(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "codex", Gate: []string{"test -f half.txt && test -f done.txt"},
		Tasks: []Task{{ID: "third", Prompt: "p"}}})
	tr := res.Tasks[0]
	var got []string
	for _, a := range tr.Attempts {
		got = append(got, a.Backend)
	}
	if tr.Status != StatusPass || strings.Join(got, ",") != "codex,claude,agy" {
		t.Fatalf("status %s, attempts %v", tr.Status, got)
	}
	if a := tr.Attempts[2]; !a.Continued || a.SwitchedFrom != "claude" {
		t.Errorf("attempt 3: continued %v, switched from %q", a.Continued, a.SwitchedFrom)
	}
}

func TestWithAPreferenceARetryMeantForASpentBackendStartsOnTheThird(t *testing.T) {
	dir := initRepo(t)
	preferThird(t, "wb-bad")
	resets := time.Now().Add(time.Hour)
	for _, name := range []string{"codex", "claude"} {
		if err := limits.Note(name, agent.Result{Status: agent.StatusLimited, LimitResets: resets}); err != nil {
			t.Fatal(err)
		}
	}
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-bad", Retry: []string{"codex"},
		Tasks: []Task{{ID: "retried", Prompt: "p"}}})
	tr := res.Tasks[0]
	if tr.Status != StatusPass || len(tr.Attempts) != 2 {
		t.Fatalf("task = %+v", tr)
	}
	if a := tr.Attempts[1]; a.Backend != "agy" || a.SwitchedFrom != "codex" {
		t.Errorf("the retry ran on %s, switched from %q", a.Backend, a.SwitchedFrom)
	}
}
