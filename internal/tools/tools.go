// Package tools is the client-side tool runtime used by the API backend.
// Every path a tool touches is resolved through a Workspace, which refuses
// anything outside the task directory.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Effect classifies what a tool can change; the permission gate keys off it.
type Effect int

const (
	EffectRead Effect = iota
	EffectWrite
	EffectExec
)

type Tool struct {
	Name        string
	Description string
	Properties  map[string]any
	Required    []string
	// Schema, when set, is the complete input schema and replaces Properties
	// and Required (MCP tools bring their own).
	Schema map[string]any
	Effect Effect
	// Summary renders a one-line description of a call for logs and approvals.
	Summary func(input json.RawMessage) string
	Run     func(ctx context.Context, ws *Workspace, input json.RawMessage) (string, error)
}

type Workspace struct {
	Root string
	// AllowDangerous lifts the command guard for this run. It lives here
	// because the tool set is built once and shared by every run, so per-task
	// policy has to travel with the workspace.
	AllowDangerous bool
}

func NewWorkspace(dir string) (*Workspace, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", abs)
	}
	return &Workspace{Root: abs}, nil
}

// Resolve maps a model-supplied path to an absolute path inside the workspace.
func (w *Workspace) Resolve(p string) (string, error) {
	if p == "" {
		p = "."
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(w.Root, p)
	}
	p = filepath.Clean(p)
	// Resolve symlinks on the longest existing prefix so a link can't escape.
	check := p
	for {
		if real, err := filepath.EvalSymlinks(check); err == nil {
			rest, _ := filepath.Rel(check, p)
			check = filepath.Join(real, rest)
			break
		}
		parent := filepath.Dir(check)
		if parent == check {
			break
		}
		check = parent
	}
	rel, err := filepath.Rel(w.Root, check)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the workspace %s", p, w.Root)
	}
	return p, nil
}

// Rel renders an absolute workspace path for display.
func (w *Workspace) Rel(p string) string {
	if rel, err := filepath.Rel(w.Root, p); err == nil {
		return filepath.ToSlash(rel)
	}
	return p
}

// All returns the default tool set in a fixed order; a stable order keeps the
// API prompt cache warm across turns.
func All() []Tool {
	return []Tool{readFile(), listFiles(), grep(), writeFile(), editFile(), runCommand()}
}

func decode[T any](input json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(input, &v); err != nil {
		return v, fmt.Errorf("invalid input: %w", err)
	}
	return v, nil
}

const maxOutput = 30_000

func Truncate(s string) string {
	if len(s) <= maxOutput {
		return s
	}
	head := s[:maxOutput*2/3]
	tail := s[len(s)-maxOutput/3:]
	return fmt.Sprintf("%s\n\n[... %d bytes truncated ...]\n\n%s", head, len(s)-len(head)-len(tail), tail)
}
