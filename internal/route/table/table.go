// Package table stores routing rows without depending on execution or eval.
// This keeps eval -> batch -> agent free of an import cycle.
package table

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type Row struct {
	Backend string `json:"backend,omitempty"`
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
	Eval    string `json:"eval,omitempty"`
	Set     string `json:"set"`
}

type Table map[string]Row

type RoutedBy struct {
	Category string `json:"category"`
	Declared bool   `json:"declared,omitempty"` // the run named its purpose; false means classified
	Eval     string `json:"eval,omitempty"`
	Set      string `json:"set"`
}

func (t Table) Pick(category string) (Row, bool) { r, ok := t[category]; return r, ok }

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
