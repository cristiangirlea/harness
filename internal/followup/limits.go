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
	"github.com/cristiangirlea/harness/internal/quota"
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

	if len(p.AccountsFor(backend)) > 0 && spent.hasAccount(backend) {
		if ok, _ := ready(p, backend, now, spent); ok {
			return backend
		}
	}
	other := planPair(p, backend, now, spent)
	if p.LocalAPI == "" || backend == "api" || !p.Enabled("api") {
		return other
	}
	if other != "" {
		if ok, _ := ready(p, other, now, spent); ok {
			return other
		}
	}
	if ok, _ := ready(p, "api", now, spent); ok || other == "" {
		return "api"
	}
	return other
}

func planPair(p prefs.Prefs, backend string, now time.Time, spent Spent) string {
	if len(p.EnabledNames) > 0 {
		first := ""
		for _, name := range p.Filter(agent.Names()) {
			if name == backend || (!p.Metered() && !planPartner(name)) {
				continue
			}
			// Partner handles the local fallback after the other candidates.
			if name == "api" && (p.LocalAPI != "" || p.BillingMaxCostUSD <= 0) {
				continue
			}
			b, _ := agent.Get(name)
			if _, err := b.Detect(); err != nil {
				continue
			}
			if ok, _ := ready(p, name, now, spent); ok {
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
	return limits.NoteAccount(backend, res.Account, res)
}

// Spent tracks task-local limits without known resets too.
type Spent map[string]bool

func (s Spent) Mark(backend, account string) {
	s[accountLabel(backend, account)] = true
}

func (s Spent) hasAccount(backend string) bool {
	for key, out := range s {
		if out && strings.HasPrefix(key, backend+"@") {
			return true
		}
	}
	return false
}

func accountLabel(backend, account string) string {
	if account == "" {
		return backend
	}
	return backend + "@" + account
}

func (s Spent) out(backend, account string, now time.Time) string {
	label := accountLabel(backend, account)
	if s[label] {
		return label + " reached its usage limit earlier in this task"
	}
	if until := limits.UntilAccount(backend, account, now); !until.IsZero() {
		return fmt.Sprintf("%s is at its usage limit until %s", label, when(until))
	}
	return ""
}

func Account(backend, asked string, now time.Time, spent Spent) (prefs.Account, bool, string) {
	p, err := resolve.BackendPreferences(runlog.Home())
	if err != nil {
		return prefs.Account{}, false, err.Error()
	}
	return chooseAccount(p, backend, asked, now, spent)
}

func chooseAccount(p prefs.Prefs, backend, asked string, now time.Time, spent Spent) (prefs.Account, bool, string) {
	accounts := p.AccountsFor(backend)
	if len(accounts) == 0 {
		if asked != "" {
			return prefs.Account{}, false, backend + " has no named accounts"
		}
		return prefs.Account{}, false, ""
	}
	if asked != "" {
		for _, a := range accounts {
			if a.Name == asked {
				return a, true, spent.out(backend, a.Name, now)
			}
		}
		return prefs.Account{}, true, fmt.Sprintf("unknown %s account", backend)
	}
	var best prefs.Account
	bestLeft := -1.0
	var skipped []string
	for _, a := range accounts {
		if why := spent.out(backend, a.Name, now); why != "" {
			skipped = append(skipped, why)
			continue
		}
		left := accountLeft(quota.ForAccount(backend, a.Name, a.Home, now), now)
		if left >= float64(p.Reserve()) {
			return a, true, strings.Join(skipped, "; ")
		}
		skipped = append(skipped, fmt.Sprintf("%s is below the %d%% reserve", accountLabel(backend, a.Name), p.Reserve()))
		if left > bestLeft {
			best, bestLeft = a, left
		}
	}
	if best.Name != "" {
		return best, true, fmt.Sprintf("%s; choosing %s with the most quota left", strings.Join(skipped, "; "), accountLabel(backend, best.Name))
	}
	return best, true, backend + " accounts are all at their usage limit: " + strings.Join(skipped, "; ")
}

func accountLeft(q quota.Quota, now time.Time) float64 {
	left := 100.0
	for _, window := range []*quota.Window{q.FiveHour, q.Week} {
		if window != nil && window.ResetsAt.After(now) {
			left = min(left, window.Left)
		}
	}
	return left
}

func AccountStatus(backend string, a prefs.Account, now time.Time) (line, usage string, until time.Time) {
	usage = quota.ForAccount(backend, a.Name, a.Home, now).String(now)
	if usage == "" {
		usage = "usage unknown"
	}
	line = a.Name + ": " + usage
	until = limits.UntilAccount(backend, a.Name, now)
	if !until.IsZero() {
		line += ", at its usage limit until " + when(until)
	}
	return line, usage, until
}

func backendOut(p prefs.Prefs, backend string, now time.Time, spent Spent) string {
	accounts := p.AccountsFor(backend)
	if len(accounts) == 0 {
		return spent.out(backend, "", now)
	}
	for _, a := range accounts {
		if spent.out(backend, a.Name, now) == "" {
			return ""
		}
	}
	return backend + " accounts are all at their usage limit"
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

// Backend applies the declared claude provider or the api endpoint and ceiling.
func Backend(name string) (agent.Backend, error) {
	return BackendFor(name, "")
}

func BackendFor(name, account string) (agent.Backend, error) {
	return backendFor(name, Plan{Account: account}, nil)
}

func (p Plan) BackendFor(name string) (agent.Backend, error) {
	if !p.defaultAccount {
		return BackendFor(name, p.Account)
	}
	return backendFor(name, p, nil)
}

func (s Spent) Backend(name string) (agent.Backend, error) {
	return backendFor(name, Plan{}, s)
}

func backendFor(name string, plan Plan, spent Spent) (agent.Backend, error) {
	b, err := agent.Get(name)
	if err != nil {
		return nil, err
	}
	if name != "api" && name != "claude" && name != "codex" && plan.Account == "" {
		return b, nil
	}
	p, err := resolve.BackendPreferences(runlog.Home())
	if name != "api" || plan.Account != "" {
		if err != nil {
			return nil, err
		}
		return plan.configureCLI(b, p, spent)
	}
	if err != nil {
		return nil, fmt.Errorf("local api: %w", err)
	}
	// Refused rather than run on the key: a continuation inherits api without asking.
	url, err := localAPI(p)
	if err != nil {
		return nil, err
	}
	api, ok := b.(*agent.APIBackend)
	if !ok {
		return b, nil
	}
	if url == "" {
		if p.Metered() && p.BillingMaxCostUSD > 0 {
			capped := *api
			capped.CostCeilingUSD = p.BillingMaxCostUSD
			return &capped, nil
		}
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

func (p Plan) ConfigureCLI(b agent.Backend, pref prefs.Prefs) (agent.Backend, error) {
	return p.configureCLI(b, pref, nil)
}

// AccountLimitError reports a remembered limit on the requested account selection.
type AccountLimitError string

func (e AccountLimitError) Error() string { return string(e) }

func (p Plan) configureCLI(b agent.Backend, pref prefs.Prefs, spent Spent) (agent.Backend, error) {
	var account prefs.Account
	if !p.defaultAccount {
		chosen, configured, why := chooseAccount(pref, b.Name(), p.Account, time.Now(), spent)
		if (p.Account != "" || configured && chosen.Name == "") && why != "" {
			if chosen.Name != "" || p.Account == "" {
				return nil, AccountLimitError(why)
			}
			return nil, errors.New(why)
		}
		account = chosen
	}
	target := pref.ProviderTarget(b.Name())
	if cli, ok := b.(*agent.CLIBackend); ok && (target != "" || account.Name != "") {
		configured := *cli
		configured.ProviderTarget = target
		configured.Account, configured.AccountHome = account.Name, account.Home
		return &configured, nil
	}
	return b, nil
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

// Ready checks whether a backend can take over under the billing policy.
func Ready(backend string, now time.Time, spent Spent) (bool, string) {
	p, err := resolve.BackendPreferences(runlog.Home())
	if backend == "api" && err != nil {
		return false, "local api: " + err.Error()
	}
	return ready(p, backend, now, spent)
}

func ready(p prefs.Prefs, backend string, now time.Time, spent Spent) (bool, string) {
	if backend == "api" {
		url, err := localAPI(p)
		if err != nil {
			return false, err.Error()
		}
		if url == "" {
			if !p.Metered() {
				return false, "api would bill per token, not to a plan"
			}
			if p.BillingMaxCostUSD <= 0 {
				return false, "api would bill per token with no ceiling; set billing_max_cost_usd (hrn backends set --max-cost)"
			}
			return planReady(p, backend, now, spent)
		}
		if why := backendOut(p, backend, now, spent); why != "" {
			return false, why
		}
		return true, ""
	}
	ok, why := planReady(p, backend, now, spent)
	if !ok {
		// An opted-in local api that is kept out explains why the chain ends here.
		if _, err := localAPI(p); err != nil {
			why += "; " + err.Error()
		}
	}
	return ok, why
}

func planReady(p prefs.Prefs, backend string, now time.Time, spent Spent) (bool, string) {
	if !p.Metered() && !planPartner(backend) {
		return false, backend + " can bill per token; automatic carry-on uses claude, codex or agy only"
	}
	b, err := agent.Get(backend)
	if err != nil {
		return false, err.Error()
	}
	if _, err := b.Detect(); err != nil {
		return false, backend + " is not installed"
	}
	if !p.Metered() {
		if name := meteredBy(backend); name != "" {
			return false, fmt.Sprintf("%s would bill per token, not to a plan, since %s is set", backend, name)
		}
	}
	if why := backendOut(p, backend, now, spent); why != "" {
		return false, why
	}
	return true, ""
}

func when(t time.Time) string { return t.Local().Format("Jan 2 15:04") }

// Pick returns backend unless it is limited and its partner is ready; bothOut means pick stays backend.
func Pick(backend string, now time.Time, spent Spent) (pick, why string, bothOut bool) {
	p, _ := resolve.BackendPreferences(runlog.Home())
	return pickPartner(p, backend, backendOut(p, backend, now, spent), now, spent)
}

func pickPartner(p prefs.Prefs, backend, gone string, now time.Time, spent Spent) (pick, why string, bothOut bool) {
	if gone == "" {
		return backend, "", false
	}
	other := Partner(backend, now, spent)
	if other == "" {
		return backend, "", false
	}
	if ok, reason := Ready(other, now, spent); !ok {
		return backend, gone + "; " + reason, backendOut(p, other, now, spent) != ""
	}
	return other, fmt.Sprintf("%s; starting on %s instead", gone, other), false
}

// Choose returns backend as typed (even empty) unless a limit moves it; a continued run is planned after.
func Choose(backend, continueID string) (string, string) {
	pick, why, _ := ChooseContinuation(Request{Backend: backend, RunID: continueID}, false, "")
	return pick, why
}

// ChooseContinuation checks the session's account before moving it to a partner.
func ChooseContinuation(req Request, noSwitch bool, freshField string) (string, string, error) {
	meant := req.Backend
	var prev runlog.Record
	if req.RunID != "" {
		prev, _ = runlog.Load(req.RunID)
		if meant == "" {
			meant = prev.Backend
		}
	}
	if meant == "" {
		meant = resolve.DefaultBackendNameIn(runlog.Home())
	}
	now := time.Now()
	p, _ := resolve.BackendPreferences(runlog.Home())
	account := prev.Result.Account
	if prev.Done && meant == prev.Backend && !req.freshFor(prev.Backend, meant) &&
		(account != "" || len(p.AccountsFor(meant)) > 0) && (req.Account == "" || req.Account == account) {
		if account != "" && !slices.ContainsFunc(p.AccountsFor(meant), func(a prefs.Account) bool { return a.Name == account }) {
			return req.Backend, "", nil
		}
		gone := Spent(nil).out(meant, account, now)
		if gone == "" {
			return req.Backend, "", nil
		}
		why := gone
		if !noSwitch && req.Account == "" {
			pick, reason, _ := pickPartner(p, meant, gone, now, nil)
			if pick != meant {
				return pick, reason, nil
			}
			if reason != "" {
				why = reason
			}
		}
		if freshField != "" {
			return req.Backend, why, fmt.Errorf("%s; use %s to continue on another account", why, freshField)
		}
		return req.Backend, why, nil
	}
	if !noSwitch && req.Account == "" {
		pick, why, _ := Pick(meant, now, nil)
		if pick != meant {
			return pick, why, nil
		}
		return req.Backend, why, nil
	}
	return req.Backend, "", nil
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

func (c CarryOn) Route() string {
	if c.Plan.Fresh {
		return c.Plan.Route()
	}
	return ""
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

	now := time.Now()
	spent := Spent{}
	spent.Mark(backend, res.Account)
	other := Partner(backend, now, spent)
	if prev.NoSwitch || other == "" {
		return nil, "", nil
	}
	if ok, reason := Ready(other, now, spent); !ok {
		return nil, why + reason + ", so the run stays " + string(agent.StatusLimited), nil
	}

	account := ""
	if other == backend {
		a, _, _ := Account(backend, "", now, spent)
		if a.Name == "" {
			return nil, why + backend + " accounts are all at their usage limit", nil
		}
		account = a.Name
	}
	plan, err := Make(Request{RunID: prevID, Backend: other, Account: account, Fresh: other == backend, Dir: prev.Dir, Retrieval: prev.Retrieval})
	if err != nil {
		return nil, "", fmt.Errorf("carrying run %s on %s: %w", prevID, other, err)
	}
	task := agent.Task{Prompt: fmt.Sprintf(carryOnPrompt, backend), Purpose: prev.Purpose, Dir: plan.Dir, Options: prev.Options, Approve: prev.Approve}
	task.Model = plan.Model
	msg := fmt.Sprintf("%s reached its usage limit", accountLabel(backend, res.Account))
	if !res.LimitResets.IsZero() {
		msg += " (until " + when(res.LimitResets) + ")"
	}
	return &CarryOn{From: backend, To: other, Task: task, Plan: plan,
		Why: fmt.Sprintf("%s%s; %s carries run %s on", why, msg, accountLabel(other, plan.Account), prevID)}, "", nil
}
