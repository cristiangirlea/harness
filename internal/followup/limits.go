package followup

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/limits"
	"github.com/cristiangirlea/harness/internal/prefs"
	"github.com/cristiangirlea/harness/internal/resolve"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// Partner prefers ready replacements, then installed ones for diagnostics, or "".
// Without a preference, claude and codex stand in for each other; an opted-in local api comes last.
func Partner(backend string, now time.Time, spent Spent) string {
	p, err := resolve.BackendPreferences(runlog.Home())
	if err != nil {
		return ""
	}
	other := planPair(p, backend, now, spent)
	if p.LocalAPI == "" || backend == "api" || !p.Enabled("api") {
		return other
	}
	if other != "" {
		if ok, _ := Ready(other, now, spent); ok {
			return other
		}
	}
	if ok, _ := Ready("api", now, spent); ok || other == "" {
		return "api"
	}
	return other
}

func planPair(p prefs.Prefs, backend string, now time.Time, spent Spent) string {
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

// apiKeys would bill per token whatever the endpoint; only their names are reported.
var apiKeys = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"}

// LocalAPI returns the opted-in local api endpoint, or why an opted-in one is kept out; both are empty without the opt-in.
func LocalAPI() (string, error) {
	p, err := resolve.BackendPreferences(runlog.Home())
	if err != nil {
		return "", fmt.Errorf("local api: %w", err)
	}
	return localAPI(p)
}

func localAPI(p prefs.Prefs) (string, error) {
	if p.LocalAPI == "" {
		return "", nil
	}
	if !p.Enabled("api") {
		return "", errors.New("local api: not used while api is disabled")
	}
	for _, name := range apiKeys {
		if os.Getenv(name) != "" {
			return "", fmt.Errorf("local api: not used while %s is set, since api would bill per token; unset it, or drop --local-api", name)
		}
	}
	return p.LocalAPI, nil
}

// Backend gets name's backend; with the local opt-in, api goes only to that endpoint or is refused.
func Backend(name string) (agent.Backend, error) {
	b, err := agent.Get(name)
	if err != nil || name != "api" {
		return b, err
	}
	// Refused rather than run on the key: a continuation inherits api without asking.
	url, err := LocalAPI()
	if err != nil {
		return nil, err
	}
	api, ok := b.(*agent.APIBackend)
	if url == "" || !ok {
		return b, nil
	}
	local := *api
	// The local server ignores the key; a proxy or a redirect could send the run elsewhere.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	local.BaseURL, local.APIKey = url, "local-api"
	local.HTTPClient = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &local, nil
}

// Detect reports a backend as runs would get it, naming an opted-in local api endpoint.
func Detect(name string) (string, error) {
	b, err := agent.Get(name)
	if err != nil {
		return "", err
	}
	// Unreadable preferences hold no usable opt-in; runs refuse them on their own.
	if p, err := resolve.BackendPreferences(runlog.Home()); err == nil && name == "api" && p.LocalAPI != "" {
		url, err := localAPI(p)
		if err != nil {
			return "", err
		}
		return "local endpoint " + url + ", the last carry-on partner", nil
	}
	return b.Detect()
}

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
		if url, err := LocalAPI(); url == "" {
			if err != nil {
				return false, err.Error()
			}
			return false, "api would bill per token, not to a plan"
		}
		if why := spent.out(backend, now); why != "" {
			return false, why
		}
		return true, ""
	}
	ok, why := planReady(backend, now, spent)
	if !ok {
		// An opted-in local api that is kept out explains why the chain ends here.
		if p, err := resolve.BackendPreferences(runlog.Home()); err == nil {
			if _, err := localAPI(p); err != nil {
				why += "; " + err.Error()
			}
		}
	}
	return ok, why
}

func planReady(backend string, now time.Time, spent Spent) (bool, string) {
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
