package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cristiangirlea/harness/internal/mcp"
)

// MCPCaller is the part of *mcp.Client the adapter needs.
type MCPCaller interface {
	Tools() []mcp.ToolInfo
	CallTool(ctx context.Context, name string, args json.RawMessage) (mcp.CallResult, error)
}

// FromMCP wraps every tool a connected server lists as a harness Tool named
// mcp__<server>__<tool>, the convention Claude Code uses. A tool the server
// marks readOnlyHint is a read (auto-approved, runs in parallel); anything
// else needs approval in ask and auto modes and is automatic only in yolo.
// The server's annotations are trusted because the user chose the server.
func FromMCP(server string, c MCPCaller) []Tool {
	var out []Tool
	for _, info := range c.Tools() {
		var schema map[string]any
		if json.Unmarshal(info.InputSchema, &schema) != nil || schema == nil {
			schema = map[string]any{}
		}
		if _, ok := schema["type"]; !ok {
			schema["type"] = "object"
		}
		if _, ok := schema["properties"]; !ok {
			schema["properties"] = map[string]any{}
		}
		effect := EffectExec
		if info.Annotations.ReadOnly() {
			effect = EffectRead
		}
		desc := info.Description
		if desc == "" {
			desc = info.Title
		}
		if desc == "" {
			desc = fmt.Sprintf("Tool %q from MCP server %q.", info.Name, server)
		}
		mcpName := info.Name
		out = append(out, Tool{
			Name:        MCPName(server, mcpName),
			Description: desc,
			Schema:      schema,
			Effect:      effect,
			Summary: func(in json.RawMessage) string {
				if s := InputSummary(in); s != "" {
					return s
				}
				return compactJSON(in, 120)
			},
			Run: func(ctx context.Context, _ *Workspace, in json.RawMessage) (string, error) {
				res, err := c.CallTool(ctx, mcpName, in)
				if err != nil {
					return "", err
				}
				text := Truncate(res.Text())
				if res.IsError {
					if text == "" {
						text = "tool reported an error"
					}
					return "", errors.New(text)
				}
				return text, nil
			},
		})
	}
	return out
}

// MCPName builds the model-facing name of an MCP tool. The Messages API only
// allows [A-Za-z0-9_-] and 128 characters.
func MCPName(server, tool string) string {
	n := "mcp__" + cleanName(server) + "__" + cleanName(tool)
	if len(n) > 128 {
		n = n[:128]
	}
	return n
}

func cleanName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// InputSummary picks the most descriptive string field of a tool input for
// logs and approval prompts. Empty when nothing familiar is there.
func InputSummary(in json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(in, &m) != nil {
		return ""
	}
	for _, k := range []string{"command", "file_path", "filePath", "path", "pattern", "url", "query", "description", "prompt"} {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func compactJSON(raw json.RawMessage, n int) string {
	var v any
	s := string(raw)
	if json.Unmarshal(raw, &v) == nil {
		b, _ := json.Marshal(v)
		s = string(b)
	}
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}
