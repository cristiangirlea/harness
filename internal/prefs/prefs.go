// Package prefs stores execution-independent backend preferences.
package prefs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

type Prefs struct {
	WorkMapSessions *bool    `json:"work_map_sessions,omitempty"`
	WorkMapGitHub   *bool    `json:"work_map_github,omitempty"`
	WorkMapRepos    []string `json:"work_map_repos,omitempty"`
	EnabledNames    []string `json:"enabled,omitempty"`
	DefaultName     string   `json:"default,omitempty"`

	Billing           string              `json:"billing,omitempty"`
	BillingMaxCostUSD float64             `json:"billing_max_cost_usd,omitempty"`
	Providers         map[string]Provider `json:"providers,omitempty"`

	Accounts              map[string][]Account `json:"accounts,omitempty"`
	AccountReservePercent *int                 `json:"account_reserve_percent,omitempty"`

	// LocalAPI opts an unmetered api endpoint in as the last carry-on partner.
	LocalAPI string `json:"local_api,omitempty"`

	Plugins *PluginPrefs `json:"plugins,omitempty"`
}

type Org struct {
	Prefs
	Locked []string `json:"locked,omitempty"`

	present bool
}

func (o Org) Exists() bool { return o.present }

func (o Org) Locks(field string) bool { return slices.Contains(o.Locked, field) }

var (
	orgPathOverride string
	orgPathTrusted  bool
)

// ErrOrganisation identifies policy errors that can be shown without a path.
var ErrOrganisation = errors.New("the organisation's backends.json")

var (
	errOrgOwner = fmt.Errorf("%w: not owned by an administrator account", ErrOrganisation)
	errOrgRead  = fmt.Errorf("%w: could not be read", ErrOrganisation)
)

func orgOpenError(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fs.ErrNotExist
	}
	return errOrgRead
}

func OrgPath() (string, error) {
	if orgPathOverride != "" {
		return orgPathOverride, nil
	}
	return systemOrgPath()
}

func LoadOrg() (Org, error) {
	const maxSize = 1 << 20
	path, err := OrgPath()
	if err != nil {
		return Org{}, err
	}
	f, err := openTrustedOrg(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Org{}, nil
	}
	if err != nil {
		return Org{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return Org{}, errOrgRead
	}
	if len(data) > maxSize {
		return Org{}, fmt.Errorf("%w: exceeds 1 MiB", ErrOrganisation)
	}
	var p *struct {
		Org
		BillingMaxCostUSD *float64        `json:"billing_max_cost_usd"`
		WorkMapSessions   json.RawMessage `json:"work_map_sessions"`
		WorkMapGitHub     json.RawMessage `json:"work_map_github"`
		WorkMapRepos      json.RawMessage `json:"work_map_repos"`
	}
	if !json.Valid(data) {
		return Org{}, fmt.Errorf("%w: malformed JSON", ErrOrganisation)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		if strings.HasPrefix(err.Error(), "json: unknown field ") {
			return Org{}, fmt.Errorf("%w: unknown field", ErrOrganisation)
		}
		return Org{}, fmt.Errorf("%w: invalid preferences object", ErrOrganisation)
	}
	if p == nil {
		return Org{}, fmt.Errorf("%w: want a preferences object", ErrOrganisation)
	}
	if p.WorkMapSessions != nil || p.WorkMapGitHub != nil || p.WorkMapRepos != nil {
		return Org{}, fmt.Errorf("%w: work-map fields are personal", ErrOrganisation)
	}
	if p.BillingMaxCostUSD != nil {
		if *p.BillingMaxCostUSD <= 0 {
			return Org{}, fmt.Errorf("%w: %w", ErrOrganisation, errBillingCeiling)
		}
		p.Prefs.BillingMaxCostUSD = *p.BillingMaxCostUSD
	}
	seen := map[string]bool{}
	for _, field := range p.Locked {
		switch field {
		case "enabled", "default", "billing", "providers", "accounts", "local_api", "plugins":
		default:
			return Org{}, fmt.Errorf("%w: unknown locked field", ErrOrganisation)
		}
		if seen[field] {
			return Org{}, fmt.Errorf("%w: duplicate locked field", ErrOrganisation)
		}
		seen[field] = true
	}
	if err := p.validate(); err != nil {
		return Org{}, fmt.Errorf("%w: %w", ErrOrganisation, withoutValues(err))
	}
	p.present = true
	return p.Org, nil
}

func Merge(org Org, user Prefs) (Prefs, []string, error) {
	p, _, ignored := combine(org, user)
	slices.Sort(ignored)
	if err := p.validate(); err != nil {
		return Prefs{}, ignored, fmt.Errorf("backend preferences: %w (after the organisation's settings)", withoutValues(err))
	}
	return p, ignored, nil
}

func (o Org) Sets(user Prefs) []string {
	_, sets, _ := combine(o, user)
	return sets
}

func combine(org Org, user Prefs) (p Prefs, sets, ignored []string) {
	p = user
	useOrg := func(field string, userSet, equal, orgSet bool) bool {
		locked := org.Locks(field)
		if locked && userSet && !equal {
			ignored = append(ignored, field)
		}
		if locked || !userSet && orgSet {
			sets = append(sets, field)
			return true
		}
		return false
	}
	if useOrg("enabled", len(user.EnabledNames) > 0, slices.Equal(user.EnabledNames, org.EnabledNames), len(org.EnabledNames) > 0) {
		p.EnabledNames = org.EnabledNames
	}
	if useOrg("default", user.DefaultName != "", user.DefaultName == org.DefaultName, org.DefaultName != "") {
		p.DefaultName = org.DefaultName
	}
	if useOrg("billing", user.Billing != "" || user.BillingMaxCostUSD != 0,
		user.Billing == org.Billing && user.BillingMaxCostUSD == org.BillingMaxCostUSD,
		org.Billing != "" || org.BillingMaxCostUSD != 0) {
		p.Billing, p.BillingMaxCostUSD = org.Billing, org.BillingMaxCostUSD
	}
	if useOrg("providers", len(user.Providers) > 0, maps.Equal(user.Providers, org.Providers), len(org.Providers) > 0) {
		p.Providers = org.Providers
	}
	if useOrg("accounts", len(user.Accounts) > 0 || user.AccountReservePercent != nil,
		maps.EqualFunc(user.Accounts, org.Accounts, slices.Equal) && equalReserve(user.AccountReservePercent, org.AccountReservePercent),
		len(org.Accounts) > 0 || org.AccountReservePercent != nil) {
		p.Accounts, p.AccountReservePercent = org.Accounts, org.AccountReservePercent
	}
	if useOrg("local_api", user.LocalAPI != "", user.LocalAPI == org.LocalAPI, org.LocalAPI != "") {
		p.LocalAPI = org.LocalAPI
	}
	if useOrg("plugins", user.Plugins != nil, equalPluginPrefs(user.Plugins, org.Plugins), org.Plugins != nil) {
		p.Plugins = org.Plugins
	}
	return p, sets, ignored
}

func equalReserve(a, b *int) bool {
	return a == b || a != nil && b != nil && *a == *b
}

type Provider struct {
	Target string `json:"target,omitempty"`
}

type Account struct {
	Name string `json:"name"`
	Home string `json:"home"`
}

var accountName = regexp.MustCompile("^[a-z0-9-]{1,32}$")

func (p Prefs) AccountsFor(backend string) []Account { return p.Accounts[backend] }

func (p Prefs) Reserve() int {
	if p.AccountReservePercent != nil {
		return *p.AccountReservePercent
	}
	return 10
}

var errBillingCeiling = errors.New("billing_max_cost_usd: want a finite positive amount")

type preferenceError struct {
	detail, safe string
}

func (e preferenceError) Error() string { return e.detail }

func withoutValues(err error) error {
	var e preferenceError
	if errors.As(err, &e) {
		return errors.New(e.safe)
	}
	return err
}

func (p Prefs) Enabled(name string) bool {
	return len(p.EnabledNames) == 0 || slices.Contains(p.EnabledNames, name)
}
func (p Prefs) Default() string { return p.DefaultName }
func (p Prefs) Metered() bool   { return p.Billing == "metered" }

func (p Prefs) ProviderTarget(backend string) string {
	target := p.Providers[backend].Target
	if target == "plan" {
		return ""
	}
	return target
}

func (p Prefs) BillingSummary() string {
	if !p.Metered() {
		return ""
	}
	if p.BillingMaxCostUSD > 0 {
		return fmt.Sprintf("billing: metered, api runs capped at $%.2f", p.BillingMaxCostUSD)
	}
	return "billing: metered, no api ceiling (api is not an automatic partner)"
}

// Filter intersects names with the preference, in preference order.
func (p Prefs) Filter(names []string) []string {
	if len(p.EnabledNames) == 0 {
		return slices.Clone(names)
	}
	filtered := []string{}
	for _, name := range p.EnabledNames {
		if slices.Contains(names, name) && !slices.Contains(filtered, name) {
			filtered = append(filtered, name)
		}
	}
	return filtered
}

// Check rejects disabled choices; empty leaves selection to the caller.
func (p Prefs) Check(name string) error {
	if name != "" && !p.Enabled(name) {
		return fmt.Errorf("backend %s is disabled; enabled: %s (hrn backends set)", name, strings.Join(p.EnabledNames, ", "))
	}
	return nil
}

func (p Prefs) validate() error {
	return p.validateUser(false)
}

func (p Prefs) validateUser(inheritsEnabled bool) error {
	if p.DefaultName != "" && !(inheritsEnabled && len(p.EnabledNames) == 0) && !slices.Contains(p.EnabledNames, p.DefaultName) {
		return preferenceError{fmt.Sprintf("default backend %s is not in enabled", p.DefaultName), "default backend is not in enabled"}
	}
	switch p.Billing {
	case "", "plan", "metered":
	default:
		return errors.New("billing: want plan or metered")
	}
	if n := p.BillingMaxCostUSD; n != 0 {
		if !p.Metered() {
			return errors.New("billing_max_cost_usd requires metered billing")
		}
		if n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
			return errBillingCeiling
		}
	}
	for backend, provider := range p.Providers {
		if backend != "claude" {
			return preferenceError{fmt.Sprintf("providers: backend %s does not support a declared target", backend), "providers: backend does not support a declared target"}
		}
		switch provider.Target {
		case "", "plan":
		case "bedrock", "vertex", "foundry", "gateway":
			if !p.Metered() {
				return preferenceError{fmt.Sprintf("providers: claude target %s bills per token and needs billing metered", provider.Target), "providers: declared target bills per token and needs billing metered"}
			}
		default:
			return preferenceError{fmt.Sprintf("providers: unknown claude target %q", provider.Target), "providers: unknown target"}
		}
	}

	if err := p.validateAccounts(); err != nil {
		return err
	}
	if err := p.Plugins.validate(); err != nil {
		return err
	}
	if p.LocalAPI != "" {
		return checkLocalAPI(p.LocalAPI)
	}
	return nil
}

func (p Prefs) validateAccounts() error {
	count := 0
	for backend, accounts := range p.Accounts {
		if backend != "claude" && backend != "codex" {
			return errors.New("accounts: only claude and codex support accounts")
		}
		if len(accounts) > 16 {
			return preferenceError{fmt.Sprintf("accounts: %s allows at most 16 accounts", backend), "accounts: at most 16 accounts per backend"}
		}
		count += len(accounts)
		seen := map[string]bool{}
		for _, a := range accounts {
			if !accountName.MatchString(a.Name) {
				return preferenceError{fmt.Sprintf("accounts: %s account names must match %s", backend, accountName), fmt.Sprintf("accounts: names must match %s", accountName)}
			}
			if seen[a.Name] {
				return preferenceError{fmt.Sprintf("accounts: duplicate %s account %s", backend, a.Name), "accounts: duplicate account name"}
			}
			if !filepath.IsAbs(a.Home) || strings.ContainsRune(a.Home, 0) {
				return preferenceError{fmt.Sprintf("accounts: %s account %s needs an absolute config home without NUL", backend, a.Name), "accounts: config homes must be absolute and contain no NUL"}
			}
			seen[a.Name] = true
		}
	}
	if p.AccountReservePercent != nil {
		if p.Reserve() < 0 || p.Reserve() > 90 {
			return errors.New("account_reserve_percent: want 0 through 90")
		}
		if count == 0 {
			return errors.New("account_reserve_percent: requires accounts")
		}
	}
	return nil
}

// checkLocalAPI accepts only hosts that cannot bill: localhost and loopback or private-network IP literals.
func checkLocalAPI(raw string) error {
	u, err := url.Parse(raw)
	// The URL is not echoed: it may carry a password.
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") {
		return errors.New("local_api: want an http or https URL with no credentials, query or fragment")
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
		return nil
	}
	return preferenceError{fmt.Sprintf("local_api: host %s is not localhost or a loopback or private-network address", u.Host), "local_api: host must be localhost or a loopback or private-network address"}
}

// Retry Windows sharing conflicts between readers and renames.
func retry(f func() error) error {
	var err error
	for i := 0; i < 20; i++ {
		if err = f(); err == nil || errors.Is(err, fs.ErrNotExist) {
			return err
		}
		time.Sleep(25 * time.Millisecond)
	}
	return err
}

// Raw preserves file bytes for change detection while retrying Windows sharing conflicts.
func Raw(home string) ([]byte, error) {
	var data []byte
	err := retry(func() (err error) { data, err = os.ReadFile(filepath.Join(home, "backends.json")); return err })
	return data, err
}

func Load(home string) (Prefs, error) {
	return loadUser(home, func() bool {
		org, _ := LoadOrg()
		return len(org.EnabledNames) > 0
	})
}

// LoadForReset keeps personal settings when a default's inherited list is unavailable.
func LoadForReset(home string) (Prefs, error) {
	return loadUser(home, func() bool { return true })
}

func (o Org) LoadUser(home string) (Prefs, error) {
	return loadUser(home, func() bool { return len(o.EnabledNames) > 0 })
}

func loadUser(home string, orgEnablesBackends func() bool) (Prefs, error) {
	path := filepath.Join(home, "backends.json")
	data, err := Raw(home)
	if os.IsNotExist(err) {
		return Prefs{}, nil
	}
	if err != nil {
		return Prefs{}, err
	}
	var p *struct {
		Prefs
		BillingMaxCostUSD *float64        `json:"billing_max_cost_usd"`
		Locked            json.RawMessage `json:"locked"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return Prefs{}, fmt.Errorf("%s: %w", path, err)
	}
	if p == nil {
		return Prefs{}, fmt.Errorf("%s: want a preferences object", path)
	}
	if p.Locked != nil {
		return Prefs{}, fmt.Errorf("%s: locked: only the organisation's file may lock fields", path)
	}
	if p.BillingMaxCostUSD != nil {
		if *p.BillingMaxCostUSD <= 0 {
			return Prefs{}, fmt.Errorf("%s: %w", path, errBillingCeiling)
		}
		p.Prefs.BillingMaxCostUSD = *p.BillingMaxCostUSD
	}
	inheritsEnabled := p.DefaultName != "" && len(p.EnabledNames) == 0 && orgEnablesBackends()
	if err := p.validateUser(inheritsEnabled); err != nil {
		return Prefs{}, fmt.Errorf("%s: %w", path, err)
	}
	return p.Prefs, nil
}

func Save(home string, p Prefs) error {
	return saveUser(home, p, false)
}

func (o Org) SaveUser(home string, p Prefs) error {
	return saveUser(home, p, len(o.EnabledNames) > 0)
}

func saveUser(home string, p Prefs, inheritsEnabled bool) error {
	if err := p.validateUser(inheritsEnabled); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(home, ".backends-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return retry(func() error { return os.Rename(f.Name(), filepath.Join(home, "backends.json")) })
}
