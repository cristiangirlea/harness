package followup

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/limits"
	"github.com/cristiangirlea/harness/internal/resolve"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// Partner prefers ready replacements, then installed ones for diagnostics, or "".
// Without a preference, claude and codex stand in for each other.
func Partner(backend string, now time.Time, spent Spent) string {
	p, err := resolve.BackendPreferences(runlog.Home())
	if err != nil {
		return ""
	}
	if len(p.EnabledNames) > 0 {
		first := ""
		for _, name := range p.Filter(agent.Names()) {
			// Automatic replacements must not bill per token.
			if name == backend || !planPartner(name) {
				continue
			}
			b, _ := agent.Get(name)
			if _, err := b.Detect(); err != nil {
				continue
			}
			if ok, _ := Ready(name, now, spent); ok {
				return name
			}
			if first == "" {
				first = name
			}
		}
		return first
	}
	switch backend {
	case "claude":
		return "codex"
	case "codex":
		return "claude"
	}
	return ""
}

func planPartner(backend string) bool {
	return backend == "claude" || backend == "codex" || backend == "agy"
}

// Note checks preferences before persisting a known limit reset.
func Note(backend string, res agent.Result) error {
	if res.Status != agent.StatusLimited || res.LimitResets.IsZero() {
		return nil
	}
	if _, err := resolve.BackendPreferences(runlog.Home()); err != nil {
		return err
	}
	return limits.Note(backend, res)
}

// Spent tracks task-local limits without known resets too.
type Spent map[string]bool

func (s Spent) out(backend string, now time.Time) string {
	if s[backend] {
		return backend + " reached its usage limit earlier in this task"
	}
	if until := limits.Until(backend, now); !until.IsZero() {
		return fmt.Sprintf("%s is at its usage limit until %s", backend, when(until))
	}
	return ""
}

// Credentials override plan billing; older codex uses OPENAI_API_KEY too.
var metered = map[string][]string{
	"claude": {"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"},
	"codex":  {"CODEX_API_KEY", "OPENAI_API_KEY"},
}

// MeteredEnv names overrides to plan billing.
func MeteredEnv(backend string) []string { return slices.Clone(metered[backend]) }

// Return names only; never expose credentials.
func meteredBy(backend string) string {
	for _, name := range metered[backend] {
		v := os.Getenv(name)
		if v == "" || strings.HasPrefix(name, "CLAUDE_CODE_USE_") && (v == "0" || strings.EqualFold(v, "false")) {
			continue
		}
		return name
	}
	return ""
}

// Ready requires plan billing for unrequested replacements.
func Ready(backend string, now time.Time, spent Spent) (bool, string) {
	if backend == "api" {
		return false, "api would bill per token, not to a plan"
	}
	if !planPartner(backend) {
		return false, backend + " can bill per token; automatic carry-on uses claude, codex or agy only"
	}
	b, err := agent.Get(backend)
	if err != nil {
		return false, err.Error()
	}
	if _, err := b.Detect(); err != nil {
		return false, backend + " is not installed"
	}
	if name := meteredBy(backend); name != "" {
		return false, fmt.Sprintf("%s would bill per token, not to a plan, since %s is set", backend, name)
	}
	if why := spent.out(backend, now); why != "" {
		return false, why
	}
	return true, ""
}

func when(t time.Time) string { return t.Local().Format("Jan 2 15:04") }

// Pick returns backend unless it is limited and its partner is ready; bothOut means pick stays backend.
func Pick(backend string, now time.Time, spent Spent) (pick, why string, bothOut bool) {
	other := Partner(backend, now, spent)
	gone := spent.out(backend, now)
	if gone == "" || other == "" {
		return backend, "", false
	}
	if ok, reason := Ready(other, now, spent); !ok {
		return backend, gone + "; " + reason, spent.out(other, now) != ""
	}
	return other, fmt.Sprintf("%s; starting on %s instead", gone, other), false
}

// Choose returns backend as typed (even empty) unless a limit moves it; a continued run is planned after.
func Choose(backend, continueID string) (string, string) {
	meant := backend
	if meant == "" && continueID != "" {
		if prev, err := runlog.Load(continueID); err == nil {
			meant = prev.Backend
		}
	}
	if meant == "" {
		meant = resolve.DefaultBackendNameIn(runlog.Home())
	}
	pick, why, _ := Pick(meant, time.Now(), nil)
	if pick != meant {
		return pick, why
	}
	return backend, why
}

const carryOnPrompt = "The turn above stopped before it finished: %s reached its plan's usage limit. " +
	"Carry on with that turn's request from where it stopped. Whatever it changed is already in this " +
	"workspace: check it, then finish the task, and answer as that turn would have."

// CarryOn plans a limited run's replacement.
type CarryOn struct {
	From, To string
	// Task must be recorded before Plan.Apply adds the transcript.
	Task agent.Task
	Plan Plan

	Why string
}

// After persists a limit and plans a switch, or returns nil.
func After(prevID, backend string, prev agent.Task, res agent.Result) (next *CarryOn, why string, err error) {
	if res.Status != agent.StatusLimited {
		return nil, "", nil
	}
	if err := Note(backend, res); err != nil {
		// Persistence failure costs only a future limited start.
		why = "the limit was not remembered: " + err.Error() + "; "
	}
	other := Partner(backend, time.Now(), nil)
	if prev.NoSwitch || other == "" {
		return nil, "", nil
	}
	if ok, reason := Ready(other, time.Now(), nil); !ok {
		return nil, why + reason + ", so the run stays " + string(agent.StatusLimited), nil
	}
	plan, err := Make(Request{RunID: prevID, Backend: other, Dir: prev.Dir, Retrieval: prev.Retrieval})
	if err != nil {
		return nil, "", fmt.Errorf("carrying run %s on %s: %w", prevID, other, err)
	}
	task := agent.Task{Prompt: fmt.Sprintf(carryOnPrompt, backend), Purpose: prev.Purpose, Dir: plan.Dir, Options: prev.Options, Approve: prev.Approve}
	task.Model = plan.Model
	msg := fmt.Sprintf("%s reached its usage limit", backend)
	if !res.LimitResets.IsZero() {
		msg += " (until " + when(res.LimitResets) + ")"
	}
	return &CarryOn{From: backend, To: other, Task: task, Plan: plan,
		Why: fmt.Sprintf("%s%s; %s carries run %s on", why, msg, other, prevID)}, "", nil
}
