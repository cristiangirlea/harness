// Package tools provides confined API tools.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Effect selects permission policy.
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
	// Schema overrides Properties/Required for MCP.
	Schema map[string]any
	Effect Effect
	// Summary supplies log/approval text.
	Summary func(input json.RawMessage) string
	// Refuse avoids forbidden-call prompts before approval; "" allows.
	Refuse func(ws *Workspace, input json.RawMessage) string
	Run    func(ctx context.Context, ws *Workspace, input json.RawMessage) (string, error)
}

type Workspace struct {
	Root string
	// RunDir is trusted, outside model control.
	RunDir     string
	ShapeTools bool
	// AllowDangerous is per-run; tools are shared.
	AllowDangerous bool
	// Commands use Root; confinement needs its junction target.
	real string
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
	return &Workspace{Root: abs, real: realPath(abs)}, nil
}

// Resolve confines model paths to the workspace.
func (w *Workspace) Resolve(p string) (string, error) {
	if p == "" {
		p = "."
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(w.Root, p)
	}
	p = filepath.Clean(p)
	real := realPath(p)
	if real == "" || w.real == "" {
		return "", fmt.Errorf("path %q is refused: hrn cannot tell where it leads (a link that loops or cannot be read, or a name the system refuses)", p)
	}
	if !w.inside(real) {
		return "", fmt.Errorf("path %q is outside the workspace %s", p, w.Root)
	}
	return p, nil
}

const maxLinks = 255

// New files need resolved prefixes, junctions and 8.3 aliases.
func realPath(p string) string {
	// Each EvalSymlinks follows 255 links; catch loops early.
	seen := map[string]bool{}
	for range maxLinks {
		seen[p] = true
		r := evalSymlinksPrefix(p)
		next, ok := throughLink(r)
		switch {
		case !ok:
			return r
		case next == "" || seen[next]:
			return ""
		}
		p = next
	}
	return ""
}

func evalSymlinksPrefix(p string) string {
	check := p
	for {
		if real, err := filepath.EvalSymlinks(check); err == nil {
			rest, _ := filepath.Rel(check, p)
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(check)
		if parent == check {
			return check
		}
		check = parent
	}
}

func (w *Workspace) inside(real string) bool {
	if real == "" {
		return false
	}
	rel, err := filepath.Rel(w.real, real)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (w *Workspace) secretPath(p string) bool {
	return SecretFile(w.Rel(p)) || w.secretReal(realPath(p))
}

// Real roots keep junction ancestors out of secret matching.
func (w *Workspace) secretReal(real string) bool {
	if real == "" {
		return false // Resolve refuses a path hrn cannot judge
	}
	rel, err := filepath.Rel(w.real, real)
	if err != nil {
		rel = real
	}
	return SecretFile(rel)
}

// Rel renders workspace paths.
func (w *Workspace) Rel(p string) string {
	if rel, err := filepath.Rel(w.Root, p); err == nil {
		return filepath.ToSlash(rel)
	}
	return p
}

// All keeps tool order for prompt caching.
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
