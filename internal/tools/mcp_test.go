package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/mcp"
)

type fakeCaller struct {
	tools  []mcp.ToolInfo
	calls  []string
	result mcp.CallResult
}

func (f *fakeCaller) Tools() []mcp.ToolInfo { return f.tools }
func (f *fakeCaller) CallTool(_ context.Context, name string, args json.RawMessage) (mcp.CallResult, error) {
	f.calls = append(f.calls, name+":"+string(args))
	return f.result, nil
}

func TestFromMCP(t *testing.T) {
	f := &fakeCaller{tools: []mcp.ToolInfo{
		{Name: "get thing", Description: "Gets.", Annotations: &mcp.Annotations{ReadOnlyHint: mcp.Bool(true)},
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"$defs":{"x":{}}}`)},
		{Name: "put", Title: "Put it", InputSchema: json.RawMessage(`{}`)},
	}}
	ts := FromMCP("my.server", f)
	if len(ts) != 2 {
		t.Fatalf("tools = %+v", ts)
	}
	get, put := ts[0], ts[1]
	if get.Name != "mcp__my_server__get_thing" || get.Effect != EffectRead || get.Description != "Gets." {
		t.Errorf("get = %+v", get)
	}
	if _, ok := get.Schema["$defs"]; !ok || get.Schema["required"] == nil {
		t.Errorf("schema not passed through whole: %v", get.Schema)
	}
	if put.Name != "mcp__my_server__put" || put.Effect != EffectExec || put.Description != "Put it" {
		t.Errorf("put = %+v", put)
	}
	if put.Schema["type"] != "object" || put.Schema["properties"] == nil {
		t.Errorf("empty schema should become an object schema: %v", put.Schema)
	}

	f.result = mcp.CallResult{Content: []mcp.Content{mcp.TextContent("ok!")}}
	out, err := get.Run(context.Background(), nil, json.RawMessage(`{"path":"a"}`))
	if err != nil || out != "ok!" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if f.calls[0] != `get thing:{"path":"a"}` {
		t.Errorf("server must be called with its own tool name: %v", f.calls)
	}

	f.result = mcp.CallResult{Content: []mcp.Content{mcp.TextContent("nope")}, IsError: true}
	if out, err := get.Run(context.Background(), nil, json.RawMessage(`{}`)); out != "" || err == nil || err.Error() != "nope" {
		t.Errorf("tool error: out=%q err=%v", out, err)
	}

	if s := get.Summary(json.RawMessage(`{"path":"a/b"}`)); s != "a/b" {
		t.Errorf("summary = %q", s)
	}
	if s := put.Summary(json.RawMessage(`{"z": 1}`)); s != `{"z":1}` {
		t.Errorf("fallback summary = %q", s)
	}
}

func TestMCPName(t *testing.T) {
	if n := MCPName("a b", "c.d/e"); n != "mcp__a_b__c_d_e" {
		t.Errorf("name = %q", n)
	}
	if n := MCPName(strings.Repeat("s", 100), strings.Repeat("t", 100)); len(n) != 128 {
		t.Errorf("len = %d", len(n))
	}
}

func TestInputSummary(t *testing.T) {
	if s := InputSummary(json.RawMessage(`{"command":"ls","path":"x"}`)); s != "ls" {
		t.Errorf("summary = %q", s)
	}
	if s := InputSummary(json.RawMessage(`{"other":1}`)); s != "" {
		t.Errorf("summary = %q, want empty", s)
	}
}
