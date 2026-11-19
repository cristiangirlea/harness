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
	"runtime"
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
}

type Org struct {
	Prefs
	Locked []string `json:"locked,omitempty"`

	present bool
}

func (o Org) Exists() bool { return o.present }

func (o Org) Locks(field string) bool { return slices.Contains(o.Locked, field) }

var orgPathOverride string

func OrgPath() string {
	if orgPathOverride != "" {
		return orgPathOverride
	}
	if runtime.GOOS != "windows" {
		return "/etc/hrn/backends.json"
	}
	root := os.Getenv("ProgramData")
	if root == "" {
		root = `C:\ProgramData`
	}
	return filepath.Join(root, "hrn", "backends.json")
}

func LoadOrg() (Org, error) {
	const label = "the organisation's backends.json"
	const maxSize = 1 << 20
	f, err := os.Open(OrgPath())
	if errors.Is(err, fs.ErrNotExist) {
		return Org{}, nil
	}
	if err != nil {
		return Org{}, errors.New(label + ": could not be read")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return Org{}, errors.New(label + ": could not be read")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return Org{}, errors.New(label + ": could not be read")
	}
	if len(data) > maxSize {
		return Org{}, errors.New(label + ": exceeds 1 MiB")
	}
	var p *struct {
		Org
		BillingMaxCostUSD *float64        `json:"billing_max_cost_usd"`
		WorkMapSessions   json.RawMessage `json:"work_map_sessions"`
		WorkMapGitHub     json.RawMessage `json:"work_map_github"`
		WorkMapRepos      json.RawMessage `json:"work_map_repos"`
	}
	if !json.Valid(data) {
		return Org{}, errors.New(label + ": malformed JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		if strings.HasPrefix(err.Error(), "json: unknown field ") {
			return Org{}, errors.New(label + ": unknown field")
		}
		return Org{}, errors.New(label + ": invalid preferences object")
	}
	if p == nil {
		return Org{}, errors.New(label + ": want a preferences object")
	}
	if p.WorkMapSessions != nil || p.WorkMapGitHub != nil || p.WorkMapRepos != nil {
		return Org{}, errors.New(label + ": work-map fields are personal")
	}
	if p.BillingMaxCostUSD != nil {
		if *p.BillingMaxCostUSD <= 0 {
			return Org{}, fmt.Errorf("%s: %w", label, errBillingCeiling)
		}
		p.Prefs.BillingMaxCostUSD = *p.BillingMaxCostUSD
	}
	seen := map[string]bool{}
	for _, field := range p.Locked {
		switch field {
		case "enabled", "default", "billing", "providers", "accounts", "local_api":
		default:
			return Org{}, errors.New(label + ": unknown locked field")
		}
		if seen[field] {
			return Org{}, errors.New(label + ": duplicate locked field")
		}
		seen[field] = true
	}
	if err := p.validate(); err != nil {
		return Org{}, fmt.Errorf("%s: %w", label, withoutValues(err))
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
	if p.DefaultName != "" && !slices.Contains(p.EnabledNames, p.DefaultName) {
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
	if err := p.validate(); err != nil {
		return Prefs{}, fmt.Errorf("%s: %w", path, err)
	}
	return p.Prefs, nil
}

func Save(home string, p Prefs) error {
	if err := p.validate(); err != nil {
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
