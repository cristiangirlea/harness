package hrnmcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/mcp"
)

// ProjectProfile is an opt-in, credential-free snapshot for a single workspace.
// Reviewed file hashes bind dirty source as well as the committed revision.
type ProjectProfile struct {
	Workspace    string            `json:"workspace"`
	Backend      string            `json:"backend"`
	Options      agent.Options     `json:"options"`
	SourceHashes map[string]string `json:"source_hashes"`
}

func (p ProjectProfile) validate(ctx context.Context, dir string) error {
	a, err := os.Stat(dir)
	if err != nil {
		return err
	}
	b, err := os.Stat(p.Workspace)
	if err != nil || !os.SameFile(a, b) {
		return fmt.Errorf("retrieval profile workspace mismatch")
	}
	if p.Backend == "" || p.Options.Retrieval != "required" || p.Options.RetrievalVersion == "" || len(p.SourceHashes) == 0 {
		return fmt.Errorf("project profile requires a backend, required retrieval, a version pin and reviewed source hashes")
	}
	if err := p.Options.Validate(); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD")
	head, err := command.Output()
	if err != nil || strings.TrimSpace(string(head)) != p.Options.RetrievalRevision {
		return fmt.Errorf("retrieval profile revision changed; review and refresh the knowledge version")
	}
	for path, want := range p.SourceHashes {
		clean := filepath.Clean(filepath.FromSlash(path))
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("invalid reviewed source path")
		}
		data, err := os.ReadFile(filepath.Join(dir, clean))
		if err != nil {
			return fmt.Errorf("reviewed source unavailable: %s", path)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != want {
			return fmt.Errorf("reviewed source changed: %s; review and refresh the knowledge version", path)
		}
	}
	return nil
}

func (p ProjectProfile) arguments(ctx context.Context, dir string, raw json.RawMessage) (json.RawMessage, error) {
	args := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	if args == nil {
		args = map[string]json.RawMessage{}
	}
	if _, ok := args["continue_run_id"]; ok {
		return nil, fmt.Errorf("project retrieval profile requires a fresh run")
	}
	// This server is dedicated to one scope; explicit off uses the same workspace.
	fixed := map[string]string{"dir": p.Workspace, "backend": p.Backend, "model": p.Options.Model, "retrieval_tenant": p.Options.RetrievalTenant, "retrieval_project": p.Options.RetrievalProject, "retrieval_revision": p.Options.RetrievalRevision, "retrieval_version": p.Options.RetrievalVersion}
	for key, want := range fixed {
		if raw, ok := args[key]; ok {
			var got string
			if err := json.Unmarshal(raw, &got); err != nil {
				return nil, err
			}
			if got != "" && got != want {
				return nil, fmt.Errorf("project retrieval profile rejects a different %s", key)
			}
		}
		args[key], _ = json.Marshal(want)
	}
	var mode string
	if value, ok := args["retrieval"]; ok {
		if err := json.Unmarshal(value, &mode); err != nil {
			return nil, err
		}
	}
	if mode == "" {
		mode = "required"
		args["retrieval"], _ = json.Marshal(mode)
	}
	if mode != "off" {
		if err := p.validate(ctx, dir); err != nil {
			return nil, err
		}
	}
	return json.Marshal(args)
}

// NewProjectServer exposes only local run/inspection tools. Batch and remote
// execution cannot silently bypass the project profile.
func NewProjectServer(dir, profilePath string) (*mcp.Server, error) {
	data, err := os.ReadFile(profilePath)
	if err != nil {
		return nil, err
	}
	var profile ProjectProfile
	if err = json.Unmarshal(data, &profile); err != nil {
		return nil, err
	}
	if err = profile.validate(context.Background(), dir); err != nil {
		return nil, err
	}
	s := mcp.NewServer(mcp.Implementation{Name: "hrn-project", Version: agent.Version}, "Project-scoped Harness with required, pinned reviewed-source retrieval. Use run for this workspace; permissions default to readonly. Profile scope cannot be changed. Explicit retrieval off disables context for a fresh baseline run. Reviewed source freshness is checked before each retrieval-enabled run. Use the regular hrn server for other workspaces and native continuation.")
	tool := runTool(dir, false)
	tool.Description += " This server supplies a fixed project scope and defaults to required retrieval."
	properties := tool.InputSchema["properties"].(map[string]any)
	delete(properties, "continue_run_id")
	properties["backend"] = map[string]any{"type": "string", "enum": []string{profile.Backend}, "default": profile.Backend, "description": "Backend fixed by the project retrieval profile."}
	properties["model"].(map[string]any)["default"] = profile.Options.Model
	properties["retrieval"].(map[string]any)["default"] = "required"
	original := tool.Handler
	tool.Handler = func(ctx context.Context, call mcp.Call) (mcp.CallResult, error) {
		args, err := profile.arguments(ctx, dir, call.Arguments)
		if err != nil {
			return mcp.CallResult{}, err
		}
		call.Arguments = args
		return original(ctx, call)
	}
	s.AddTool(tool)
	s.AddTool(runsTool())
	s.AddTool(showTool())
	s.AddTool(backendsTool())
	return s, nil
}
