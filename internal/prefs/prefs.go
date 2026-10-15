// Package prefs stores backend preferences without depending on execution or records.
package prefs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type Prefs struct {
	EnabledNames []string `json:"enabled,omitempty"`
	DefaultName  string   `json:"default,omitempty"`
}

func (p Prefs) Enabled(name string) bool {
	return len(p.EnabledNames) == 0 || slices.Contains(p.EnabledNames, name)
}
func (p Prefs) Default() string { return p.DefaultName }

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

// Check refuses a disabled explicit choice; an empty name leaves selection to the caller.
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
	return nil
}

func Load(home string) (Prefs, error) {
	path := filepath.Join(home, "backends.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Prefs{}, nil
	}
	if err != nil {
		return Prefs{}, err
	}
	var p *Prefs
	if err := json.Unmarshal(data, &p); err != nil {
		return Prefs{}, fmt.Errorf("%s: %w", path, err)
	}
	if p == nil {
		return Prefs{}, fmt.Errorf("%s: want a preferences object", path)
	}
	if err := p.validate(); err != nil {
		return Prefs{}, fmt.Errorf("%s: %w", path, err)
	}
	return *p, nil
}

func Save(home string, p Prefs) error {
	if err := p.validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0755); err != nil {
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
	return os.Rename(f.Name(), filepath.Join(home, "backends.json"))
}
