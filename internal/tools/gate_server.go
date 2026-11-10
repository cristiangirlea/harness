package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"

	"github.com/cristiangirlea/harness/internal/mcp"
)

// GatePolicySum is the policy file's SHA-256, which the gate's tool server checks before each command.
func GatePolicySum(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// NewGateServer offers a run's command tools over MCP, for a CLI whose own shell tools are off.
// policySum is the run's policy as hrn wrote it; a command runs only while the file still matches.
func NewGateServer(ws *Workspace, version string, shape bool, policySum string) *mcp.Server {
	s := mcp.NewServer(mcp.Implementation{Name: "hrn-gate", Version: version},
		"Run every command with run_command: this run's own shell tools are off.")
	readOnly := true
	s.AddTool(gateTool(ws, runCommand(), nil, func(raw json.RawMessage) string { return gateServerRefusal(ws, policySum, raw) }))
	if shape {
		s.AddTool(gateTool(ws, ReadOriginal(), &mcp.Annotations{ReadOnlyHint: &readOnly}, nil))
	}
	return s
}

// gateServerRefusal keeps hrn's guard whatever the policy says: the CLI's own file tools can write
// the policy file, so neither the file nor the gate reading it is trusted alone.
func gateServerRefusal(ws *Workspace, policySum string, raw json.RawMessage) string {
	if policySum != "" {
		if sum, err := GatePolicySum(ws.GatePolicy); err != nil || sum != policySum {
			return "refused: the run's gate policy file was changed or removed, so no command runs; say so in your answer"
		}
	}
	var args struct {
		Command string `json:"command"`
	}
	if ws.AllowDangerous || json.Unmarshal(raw, &args) != nil {
		return ""
	}
	if rule := Guard(args.Command); rule != "" {
		return GuardError(rule).Error()
	}
	return ""
}

func gateTool(ws *Workspace, t Tool, annotations *mcp.Annotations, refuse func(json.RawMessage) string) mcp.Tool {
	schema := map[string]any{"type": "object", "properties": t.Properties}
	if len(t.Required) > 0 {
		schema["required"] = t.Required
	}
	return mcp.Tool{Name: t.Name, Description: t.Description, InputSchema: schema, Annotations: annotations,
		Handler: func(ctx context.Context, call mcp.Call) (mcp.CallResult, error) {
			if refuse != nil {
				if text := refuse(call.Arguments); text != "" {
					return gateResult(text, true), nil
				}
			}
			if t.Refuse != nil {
				if rule := t.Refuse(ws, call.Arguments); rule != "" {
					return gateResult(GuardError(rule).Error(), true), nil
				}
			}
			output, err := t.Run(ctx, ws, call.Arguments)
			if err == nil {
				return gateResult(output, false), nil
			}
			// A failed command's output is what the model needs to fix it.
			if !strings.Contains(output, err.Error()) {
				output = strings.TrimLeft(output+"\n"+err.Error(), "\n")
			}
			return gateResult(output, true), nil
		}}
}

func gateResult(text string, isError bool) mcp.CallResult {
	return mcp.CallResult{Content: []mcp.Content{{Type: "text", Text: text}}, IsError: isError}
}
