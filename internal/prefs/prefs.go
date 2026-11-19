// Package prefs stores execution-independent backend preferences.
package prefs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
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

	// LocalAPI opts an unmetered api endpoint in as the last carry-on partner.
	LocalAPI string `json:"local_api,omitempty"`
}

type Provider struct {
	Target string `json:"target,omitempty"`
}

var errBillingCeiling = errors.New("billing_max_cost_usd: want a finite positive amount")

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
		return fmt.Errorf("default backend %s is not in enabled", p.DefaultName)
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
			return fmt.Errorf("providers: backend %s does not support a declared target", backend)
		}
		switch provider.Target {
		case "", "plan":
		case "bedrock", "vertex", "foundry", "gateway":
			if !p.Metered() {
				return fmt.Errorf("providers: claude target %s bills per token and needs billing metered", provider.Target)
			}
		default:
			return fmt.Errorf("providers: unknown claude target %q", provider.Target)
		}
	}
	if p.LocalAPI != "" {
		return checkLocalAPI(p.LocalAPI)
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
	return fmt.Errorf("local_api: host %s is not localhost or a loopback or private-network address", u.Host)
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
		BillingMaxCostUSD *float64 `json:"billing_max_cost_usd"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return Prefs{}, fmt.Errorf("%s: %w", path, err)
	}
	if p == nil {
		return Prefs{}, fmt.Errorf("%s: want a preferences object", path)
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
