package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cristiangirlea/harness/internal/mcp"
)

// MCPCaller supplies tools and calls.
type MCPCaller interface {
	Tools() []mcp.ToolInfo
	CallTool(ctx context.Context, name string, args json.RawMessage) (mcp.CallResult, error)
}

// FromMCP trusts chosen servers' readOnlyHint for parallelism/approval.
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
			mcpServer:   server,
			mcpTool:     mcpName,
			Description: desc,
			Schema:      schema,
			Effect:      effect,
			Summary: func(in json.RawMessage) string {
				if s := InputSummary(in); s != "" {
					return s
				}
				return compactJSON(in, 120)
			},
			Run: func(ctx context.Context, ws *Workspace, in json.RawMessage) (string, error) {
				res, err := c.CallTool(ctx, mcpName, in)
				if err != nil {
					return "", err
				}
				text := ws.toolOutput(res.Text())
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

// MCPRename preserves both identities so collisions can be explained to the user.
type MCPRename struct {
	Server, Tool                   string
	CollidingServer, CollidingTool string
	OldName, NewName               string
}

// UniqueMCPNames reserves original names so suffixes cannot displace later tools.
func UniqueMCPNames(ts []Tool) (out []Tool, renamed []MCPRename) {
	out = append([]Tool(nil), ts...)
	owners := make(map[string]int, len(ts))
	for i, t := range ts {
		if _, exists := owners[t.Name]; !exists {
			owners[t.Name] = i
		}
	}
	for i, t := range ts {
		owner := owners[t.Name]
		if owner == i {
			continue
		}
		identity := t.mcpServer + "\x00" + t.mcpTool
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
		for attempt, width := 0, 8; ; width++ {
			if width > len(digest) {
				// Repeated identical entries can exhaust every prefix of the same hash.
				attempt++
				digest = fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", identity, attempt))))
				width = 8
			}
			base := t.Name
			if len(base) > 127-width {
				base = base[:127-width]
			}
			name := base + "_" + digest[:width]
			if _, exists := owners[name]; exists {
				continue
			}
			owners[name] = i
			out[i].Name = name
			renamed = append(renamed, MCPRename{
				Server: t.mcpServer, Tool: t.mcpTool,
				CollidingServer: ts[owner].mcpServer, CollidingTool: ts[owner].mcpTool,
				OldName: t.Name, NewName: name,
			})
			break
		}
	}
	return out, renamed
}

// MCPName obeys API limits: [A-Za-z0-9_-], 128 characters.
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

// InputSummary picks log/approval text, or "".
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
