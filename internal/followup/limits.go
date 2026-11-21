package followup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/kernel"
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

// Pick returns backend unless it is limited or nearly spent and its partner is ready; bothOut means pick stays backend.
func Pick(backend string, now time.Time, spent Spent) (pick, why string, bothOut bool) {
	return PickKeeping(backend, now, spent, "")
}

// PickKeeping is Pick for work that a nearly spent plan must not move; keep says why ("as named"), and "" allows the move.
func PickKeeping(backend string, now time.Time, spent Spent, keep string) (pick, why string, bothOut bool) {
	p, _ := resolve.BackendPreferences(runlog.Home())
	if gone := backendOut(p, backend, now, spent); gone != "" {
		return pickPartner(p, backend, gone, now, spent)
	}
	low := quotaLow(p, backend, now)
	switch {
	case low == "":
		return backend, "", false
	case keep != "":
		return backend, low + "; running on " + backend + " " + keep, false
	}
	other := Partner(backend, now, spent)
	if other == "" || other == backend {
		return backend, low + "; no partner to start on", false
	}
	if ok, reason := Ready(other, now, spent); !ok {
		return backend, low + "; staying on " + backend + ": " + reason, false
	}
	if otherLow := quotaLow(p, other, now); otherLow != "" {
		return backend, low + "; staying on " + backend + ": " + otherLow, false
	}
	return other, low + "; starting on " + other + " instead", false
}

// lowQuotaPercent: below this much left in a current window, a run starts on the partner.
const lowQuotaPercent = 5

// planQuota is quota.For, replaced in tests.
var planQuota = quota.For

// quotaLow says which current window of backend's plan is nearly spent, or "" when none is known to be.
// Accounts choose among themselves by their reserve, and an unknown or stale window never counts.
func quotaLow(p prefs.Prefs, backend string, now time.Time) string {
	if len(p.AccountsFor(backend)) > 0 {
		return ""
	}
	q := planQuota(backend, now)
	// A reset further off than its window can last is a misread figure.
	for _, w := range []struct {
		label string
		w     *quota.Window
		span  time.Duration
	}{{"5h", q.FiveHour, 6 * time.Hour}, {"weekly", q.Week, kernel.MaxLimitWindow}} {
		if w.w == nil || !w.w.ResetsAt.After(now) || w.w.ResetsAt.After(now.Add(w.span)) {
			continue
		}
		if w.w.Left < lowQuotaPercent {
			return fmt.Sprintf("%s has %.1f%% of its %s window left, under %d%%", backend, w.w.Left, w.label, lowQuotaPercent)
		}
	}
	return ""
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

// keep says why a nearly spent plan must not move this run, or "".
func (req Request) keep() string {
	if req.Named {
		return "as named"
	}
	if req.RunID == "" || req.Retrieval == "off" {
		return ""
	}
	chain, _, err := runlog.Chain(req.RunID)
	if err != nil {
		return ""
	}
	for i := len(chain) - 1; i >= 0; i-- {
		if chain[i].Result.Retrieval != nil {
			return "to keep its retrieval pin"
		}
		// A fresh hand-over left the pins before it behind.
		if chain[i].ContinueRoute == runlog.RouteFreshHandover {
			break
		}
	}
	return ""
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
		pick, why, _ := PickKeeping(meant, now, nil, req.keep())
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
	// Until is set on a carry-on that waits for a remembered reset; Wait plans it then, and Waiting says so before.
	Until   time.Time
	Waiting string

	budget *Budget
	req    Request
	prev   agent.Task
}

func (c CarryOn) Route() string {
	if c.Plan.Fresh {
		return c.Plan.Route()
	}
	return ""
}

// Wait sleeps until the reset a waiting carry-on is for, then plans it; any other carry-on returns at once.
func (c *CarryOn) Wait(ctx context.Context) error {
	if c.Until.IsZero() {
		return nil
	}
	if err := Wait(ctx, c.Until, c.budget); err != nil {
		return err
	}
	if err := Usable(c.To, c.req.Account); err != nil {
		return err
	}
	return c.plan()
}

func (c *CarryOn) plan() error {
	plan, err := Make(c.req)
	if err != nil && c.req.Backend == c.From && !c.req.Fresh {
		// No session to resume on the same backend: hand the record over instead.
		c.req.Fresh = true
		plan, err = Make(c.req)
	}
	if err != nil {
		return fmt.Errorf("carrying run %s on %s: %w", c.req.RunID, c.To, err)
	}
	c.Plan = plan
	c.Task = agent.Task{Prompt: fmt.Sprintf(carryOnPrompt, c.From), Purpose: c.prev.Purpose, Dir: plan.Dir, Options: c.prev.Options, Approve: c.prev.Approve}
	c.Task.Model = plan.Model
	return nil
}

// Waits is what all of one run's waits share: one budget, and KeepAccount for a run given its account.
type Waits struct {
	Budget      *Budget
	KeepAccount bool
}

// After persists a limit and plans a switch, or returns nil; with wait_limit set and no partner ready, it plans a wait.
func After(prevID, backend string, prev agent.Task, res agent.Result, w Waits) (next *CarryOn, why string, err error) {
	if res.Status != agent.StatusLimited {
		return nil, "", nil
	}
	if err := Note(backend, res); err != nil {
		// Persistence failure costs only a future limited start; the error can name a path, so it is not repeated.
		why = "the usage-limit reset could not be saved; "
	}
	if w.Budget == nil {
		w.Budget = NewBudget(time.Duration(prev.WaitLimit))
	}
	pinned := prev.NoSwitch || w.KeepAccount
	// A retrieval pin binds the run to its backend, so a partner's sooner reset is no use to it.
	retrievalPinned := prev.Retrieval != "" && prev.Retrieval != "off"

	now := time.Now()
	spent := Spent{}
	spent.Mark(backend, res.Account)
	msg := fmt.Sprintf("%s reached its usage limit", accountLabel(backend, res.Account))
	if !res.LimitResets.IsZero() {
		msg += " (until " + when(res.LimitResets) + ")"
	}
	other := Partner(backend, now, spent)
	if prev.NoSwitch || other == "" {
		if prev.WaitLimit <= 0 {
			return nil, "", nil
		}
		return waitCarry(prevID, backend, prev, res, why+msg, w.Budget, WaitScope{Backend: backend, Account: res.Account, Pinned: pinned})
	}
	if ok, reason := Ready(other, now, spent); !ok {
		return waitCarry(prevID, backend, prev, res, why+reason+", so the run stays "+string(agent.StatusLimited), w.Budget,
			WaitScope{Backend: backend, Account: res.Account, Pinned: pinned, Partner: !pinned && !retrievalPinned})
	}

	account := ""
	if other == backend {
		a, _, _ := Account(backend, "", now, spent)
		if a.Name == "" {
			return waitCarry(prevID, backend, prev, res, why+backend+" accounts are all at their usage limit", w.Budget, WaitScope{Backend: backend, Account: res.Account, Pinned: pinned})
		}
		account = a.Name
	}
	c := &CarryOn{From: backend, To: other, prev: prev,
		req: Request{RunID: prevID, Backend: other, Account: account, Fresh: other == backend, Dir: prev.Dir, Retrieval: prev.Retrieval}}
	if err := c.plan(); err != nil {
		return nil, "", err
	}
	c.Why = fmt.Sprintf("%s%s; %s carries run %s on", why, msg, accountLabel(other, c.Plan.Account), prevID)
	return c, "", nil
}

// waitCarry plans a carry-on after the soonest reset scope allows, or says why the run stays limited.
func waitCarry(prevID, backend string, prev agent.Task, res agent.Result, why string, b *Budget, scope WaitScope) (*CarryOn, string, error) {
	if prev.WaitLimit <= 0 {
		return nil, why, nil
	}
	now := time.Now()
	spent := Spent{}
	spent.Mark(backend, res.Account)
	r := scope.soonest(now, spent)
	if err := waitable(r.Until, b, now); err != nil {
		return nil, why + "; not waiting: " + err.Error(), nil
	}
	label := accountLabel(r.Backend, r.Account)
	c := &CarryOn{From: backend, To: r.Backend, Until: r.Until, budget: b, prev: prev,
		req: Request{RunID: prevID, Backend: r.Backend, Account: r.Account, Fresh: r.Backend == backend && r.Account != res.Account, Dir: prev.Dir, Retrieval: prev.Retrieval}}
	c.Waiting = fmt.Sprintf("%s; waiting for %s", why, r)
	c.Why = fmt.Sprintf("%s; waited for %s; %s carries run %s on", why, r, label, prevID)
	return c, "", nil
}
