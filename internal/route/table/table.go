// Package table stores routing rows without depending on execution or eval.
// This keeps eval -> batch -> agent free of an import cycle.
package table

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type Row struct {
	Backend string `json:"backend,omitempty"`
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
	Eval    string `json:"eval,omitempty"`
	Set     string `json:"set"`
	// A later attempt on another backend uses that backend's entry instead of its default.
	Fallbacks []Row `json:"fallbacks,omitempty"`
}

type Table map[string]Row

type RoutedBy struct {
	Category string `json:"category"`
	Declared bool   `json:"declared,omitempty"` // the run named its purpose; false means classified
	Fallback bool   `json:"fallback,omitempty"` // a fallback row chose the model, not the first choice
	Eval     string `json:"eval,omitempty"`
	Set      string `json:"set"`
}

func (t Table) Pick(category string) (Row, bool) { r, ok := t[category]; return r, ok }

// FallbackFor ignores an entry for the row's own backend: no later attempt lands there as a fallback.
func (r Row) FallbackFor(backend string) (Row, bool) {
	for _, f := range r.Fallbacks {
		if f.Backend == backend && backend != r.Backend {
			return f, true
		}
	}
	return Row{}, false
}

// WithFallback replaces the entry for f's backend; a backend-only entry records that backend's defaults as the choice.
func (r Row) WithFallback(f Row) Row {
	r = r.WithoutFallback(f.Backend)
	f.Fallbacks = nil
	r.Fallbacks = append(r.Fallbacks, f)
	sort.Slice(r.Fallbacks, func(i, j int) bool { return r.Fallbacks[i].Backend < r.Fallbacks[j].Backend })
	return r
}

// WithoutFallback copies the list, so the row it was called on keeps its own.
func (r Row) WithoutFallback(backend string) Row {
	var kept []Row
	for _, old := range r.Fallbacks {
		if old.Backend != backend {
			kept = append(kept, old)
		}
	}
	r.Fallbacks = kept
	return r
}

func Load(home string) (Table, error) {
	path := filepath.Join(home, "route.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Table{}, nil
	}
	if err != nil {
		return nil, err
	}
	t := Table{}
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if t == nil {
		return nil, fmt.Errorf("%s: want an object keyed by purpose", path)
	}
	return t, nil
}

func Save(home string, t Table) error {
	if t == nil {
		t = Table{}
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(home, ".route-*.json")
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
	return os.Rename(f.Name(), filepath.Join(home, "route.json"))
}
