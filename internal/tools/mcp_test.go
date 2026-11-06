package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
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

func TestUniqueMCPNamesPreservesTheFirstNameAndStablyRenamesCollisions(t *testing.T) {
	for _, tc := range []struct{ name, server1, tool1, server2, tool2 string }{
		{"sanitized tools", "s", "get.item", "s", "get_item"},
		{"server separator", "a__b", "c", "a", "b__c"},
		{"truncated tools", "s", strings.Repeat("x", 130) + "a", "s", strings.Repeat("x", 130) + "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := append(FromMCP(tc.server1, &fakeCaller{tools: []mcp.ToolInfo{{Name: tc.tool1}}}),
				FromMCP(tc.server2, &fakeCaller{tools: []mcp.ToolInfo{{Name: tc.tool2}}})...)
			got, renamed := UniqueMCPNames(input)
			again, _ := UniqueMCPNames(input)
			digest := fmt.Sprintf("%x", sha256.Sum256([]byte(tc.server2+"\x00"+tc.tool2)))
			base := input[1].Name
			if len(base) > 119 {
				base = base[:119]
			}
			if got[0].Name != input[0].Name || got[1].Name != base+"_"+digest[:8] || got[1].Name != again[1].Name {
				t.Fatalf("names = %q, %q; repeated = %q", got[0].Name, got[1].Name, again[1].Name)
			}
			if input[1].Name != input[0].Name || len(renamed) != 1 {
				t.Fatalf("input changed or missing rename: %v", renamed)
			}
			for _, tool := range got {
				if len(tool.Name) > 128 || !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(tool.Name) {
					t.Errorf("invalid name %q", tool.Name)
				}
			}
		})
	}
}

func TestUniqueMCPNamesLeavesNonCollidingNamesUnchanged(t *testing.T) {
	input := FromMCP("s", &fakeCaller{tools: []mcp.ToolInfo{{Name: "one"}, {Name: "two"}}})
	got, renamed := UniqueMCPNames(input)
	if len(renamed) != 0 || got[0].Name != input[0].Name || got[1].Name != input[1].Name {
		t.Fatalf("unexpected renames: %v, %v", got, renamed)
	}
}

func TestUniqueMCPNamesExtendsTheHashWhenASuffixIsAlreadyTaken(t *testing.T) {
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("s\x00get_item")))
	input := FromMCP("s", &fakeCaller{tools: []mcp.ToolInfo{{Name: "get.item"}, {Name: "get_item"}, {Name: "get_item_" + digest[:8]}}})
	got, renamed := UniqueMCPNames(input)
	if got[1].Name != input[1].Name+"_"+digest[:9] || got[2].Name != input[2].Name || len(renamed) != 1 {
		t.Fatalf("names = %q, %q, %q; renames = %v", got[0].Name, got[1].Name, got[2].Name, renamed)
	}
}

func TestUniqueMCPNamesHandlesRepeatedIdenticalToolsAfterTheHashIsExhausted(t *testing.T) {
	input := FromMCP("s", &fakeCaller{tools: make([]mcp.ToolInfo, 60)})
	got, renamed := UniqueMCPNames(input)
	again, _ := UniqueMCPNames(input)
	seen := map[string]bool{}
	for i, tool := range got {
		if seen[tool.Name] || tool.Name != again[i].Name || len(tool.Name) > 128 {
			t.Fatalf("invalid or unstable name %q", tool.Name)
		}
		seen[tool.Name] = true
	}
	if len(renamed) != len(input)-1 {
		t.Fatalf("renames = %d", len(renamed))
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
