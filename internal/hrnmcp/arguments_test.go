package hrnmcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/mcp"
)

func argumentTools(t *testing.T) []mcp.Tool {
	t.Helper()
	t.Setenv("HRN_HOME", t.TempDir())
	dir := t.TempDir()
	return []mcp.Tool{runTool(dir, false), runsTool(), showTool(), flowTool(),
		batchShowTool(), batchMergeTool(), batchTool(dir, false), runTool(dir, true)}
}

func TestToolsRefuseUnknownArgumentsBeforeStartingWork(t *testing.T) {
	for _, tool := range argumentTools(t) {
		t.Run(tool.Name, func(t *testing.T) {
			result, err := tool.Handler(context.Background(), mcp.Call{Arguments: json.RawMessage(`{"misspelt_argument":true}`)})
			message := result.Text()
			if err != nil {
				message = err.Error()
			}
			if (err == nil && !result.IsError) || !strings.Contains(message, `invalid arguments: unknown field "misspelt_argument"`) {
				t.Fatalf("got result %v, error %v; want unknown field refusal", result, err)
			}
		})
	}
}

func TestToolArgumentsAcceptEveryOfferedSchemaProperty(t *testing.T) {
	for _, tool := range argumentTools(t) {
		t.Run(tool.Name, func(t *testing.T) {
			args := map[string]any{}
			for name, property := range tool.InputSchema["properties"].(map[string]any) {
				switch property.(map[string]any)["type"] {
				case "string":
					args[name] = ""
				case "integer", "number":
					args[name] = 0
				case "boolean":
					args[name] = false
				case "array":
					args[name] = []any{}
				case "object":
					args[name] = map[string]any{}
				default:
					t.Fatalf("unsupported schema type for %s: %v", name, property)
				}
			}
			if _, ok := args["compact_window"]; ok {
				args["compact_window"] = "auto"
			}
			if _, ok := args["timeout"]; ok {
				args["timeout"] = "0s"
			}
			raw, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			// Zero values stop execution after the handler's actual argument struct is decoded.
			result, err := tool.Handler(context.Background(), mcp.Call{Arguments: raw})
			if err != nil && strings.HasPrefix(err.Error(), "invalid arguments:") {
				t.Fatalf("schema properties do not decode: %v", err)
			}
			if result.IsError && strings.Contains(result.Text(), "invalid arguments:") {
				t.Fatalf("schema properties do not decode: %s", result.Text())
			}
		})
	}
}

func TestToolSchemasRefuseAdditionalProperties(t *testing.T) {
	for _, tool := range argumentTools(t) {
		t.Run(tool.Name, func(t *testing.T) {
			if value, ok := tool.InputSchema["additionalProperties"].(bool); !ok || value {
				t.Fatal("additionalProperties must be false")
			}
		})
	}
}

func TestDecodeRefusesTrailingJSONContent(t *testing.T) {
	for _, raw := range []string{`{} {}`, `{} null`, `{} garbage`} {
		t.Run(raw, func(t *testing.T) {
			if _, err := decode[struct{}](json.RawMessage(raw)); err == nil || !strings.HasPrefix(err.Error(), "invalid arguments:") {
				t.Fatalf("got %v; want invalid arguments", err)
			}
		})
	}
}

func TestDecodeKeepsEmptyArgumentsAtTheirZeroValue(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, {}, json.RawMessage("null"), json.RawMessage("{} \n\t")} {
		value, err := decode[struct{ Limit int }](raw)
		if err != nil || value.Limit != 0 {
			t.Fatalf("got %v, %v; want zero value without error", value, err)
		}
	}
}

func TestRunAndRunsRefuseCaseFoldedArgumentNames(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	name := agent.DefaultBackendName()
	old, _ := agent.Get(name)
	agent.Register(namedRouteStub{name})
	t.Cleanup(func() { agent.Register(old) })
	for _, tool := range []mcp.Tool{runTool(t.TempDir(), false), runsTool()} {
		for _, raw := range []string{`{"prompt":"x","Prompt":"y"}`, `{"Prompt":"x"}`} {
			t.Run(tool.Name+"/"+raw, func(t *testing.T) {
				result, err := tool.Handler(context.Background(), mcp.Call{Arguments: json.RawMessage(raw), Progress: func(string) {}})
				message := result.Text()
				if err != nil {
					message = err.Error()
				}
				if (err == nil && !result.IsError) || !strings.Contains(message, `invalid arguments: unknown field "Prompt"`) {
					t.Fatalf("got result %v, error %v; want exact field name refusal", result, err)
				}
			})
		}
	}
}
