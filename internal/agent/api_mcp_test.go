package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/mcp"
)

// TestHelperMCPServer is not a test: with HRN_MCP_HELPER=1 the test binary
// serves a two-tool MCP server on stdio for the tests below.
func TestHelperMCPServer(t *testing.T) {
	if os.Getenv("HRN_MCP_HELPER") != "1" {
		return
	}
	s := mcp.NewServer(mcp.Implementation{Name: "fake", Version: "1"}, "")
	s.AddTool(mcp.Tool{
		Name: "echo", Description: "Echo text.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []any{"text"}},
		Annotations: &mcp.Annotations{ReadOnlyHint: mcp.Bool(true)},
		Handler: func(_ context.Context, call mcp.Call) (mcp.CallResult, error) {
			var a struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(call.Arguments, &a)
			// The guard's marker too, as the server sees it.
			return mcp.CallResult{Content: []mcp.Content{mcp.TextContent("echo: " + a.Text + " [HRN_GUARDED=" + os.Getenv("HRN_GUARDED") + "]")}}, nil
		},
	})
	s.AddTool(mcp.Tool{
		Name: "mutate", Description: "Changes things.",
		Handler: func(context.Context, mcp.Call) (mcp.CallResult, error) {
			return mcp.CallResult{Content: []mcp.Content{mcp.TextContent("mutated")}}, nil
		},
	})
	_ = s.Serve(context.Background(), os.Stdin, os.Stdout)
	os.Exit(0)
}

func mcpConfigFile(t *testing.T, command string) string {
	t.Helper()
	cfg := map[string]any{"mcpServers": map[string]any{"fake": map[string]any{
		"command": command,
		"args":    []string{"-test.run=^TestHelperMCPServer$", "--"},
		"env":     map[string]string{"HRN_MCP_HELPER": "1"},
	}}}
	data, _ := json.Marshal(cfg)
	p := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func toolNames(req map[string]any) []string {
	var names []string
	for _, tl := range req["tools"].([]any) {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	return names
}

func TestAPILoopUsesMCPTools(t *testing.T) {
	res, events, fake := runFake(t, [][]string{
		toolTurn("toolu_m", "mcp__fake__echo", `{"text":"hi"}`),
		textTurn("done"),
	}, Task{Prompt: "use mcp", Dir: t.TempDir(), Options: Options{Mode: ModeAsk, MCP: []string{mcpConfigFile(t, os.Args[0])}}})
	if res.Status != StatusOK || res.Final != "done" {
		t.Fatalf("result: %+v", res)
	}
	block := lastToolResult(t, fake.requests[1])
	if block["is_error"] == true || !strings.Contains(fmt.Sprint(block["content"]), "echo: hi") {
		t.Errorf("mcp result not returned to the model: %v", block)
	}
	names := strings.Join(toolNames(fake.requests[0]), ",")
	for _, want := range []string{"read_file", "mcp__fake__echo", "mcp__fake__mutate"} {
		if !strings.Contains(names, want) {
			t.Errorf("tool %s not offered: %s", want, names)
		}
	}
	var infos []string
	for _, e := range events {
		if e.Kind == EventInfo {
			infos = append(infos, e.Text)
		}
	}
	if len(infos) != 1 || !strings.HasPrefix(infos[0], "mcp fake: 2 tools") {
		t.Errorf("info events = %v", infos)
	}
}

func TestAPILoopMCPModes(t *testing.T) {
	cfg := mcpConfigFile(t, os.Args[0])
	_, _, fake := runFake(t, [][]string{textTurn("ok")}, Task{Prompt: "look", Dir: t.TempDir(), Options: Options{Mode: ModeReadOnly, MCP: []string{cfg}}})
	names := strings.Join(toolNames(fake.requests[0]), ",")
	if !strings.Contains(names, "mcp__fake__echo") || strings.Contains(names, "mcp__fake__mutate") {
		t.Errorf("readonly should offer only read-only MCP tools: %s", names)
	}

	_, _, fake = runFake(t, [][]string{
		toolTurn("toolu_x", "mcp__fake__mutate", `{}`),
		textTurn("ok"),
	}, Task{Prompt: "change", Dir: t.TempDir(), Options: Options{Mode: ModeAsk, MCP: []string{cfg}}})
	if block := lastToolResult(t, fake.requests[1]); block["is_error"] != true || !strings.Contains(fmt.Sprint(block["content"]), "Permission denied") {
		t.Errorf("non-read-only MCP tool must need approval in ask mode: %v", block)
	}
}

func TestAPILoopMCPStartFailureIsReported(t *testing.T) {
	res, events, fake := runFake(t, [][]string{textTurn("ok")},
		Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Mode: ModeAsk, MCP: []string{mcpConfigFile(t, "hrn-definitely-missing-binary")}}})
	if res.Status != StatusOK {
		t.Fatalf("a broken MCP server must not fail the run: %+v", res)
	}
	var errs []string
	for _, e := range events {
		if e.Kind == EventError {
			errs = append(errs, e.Text)
		}
	}
	if len(errs) != 1 || !strings.HasPrefix(errs[0], "mcp fake:") {
		t.Errorf("error events = %v", errs)
	}
	if names := strings.Join(toolNames(fake.requests[0]), ","); strings.Contains(names, "mcp__") {
		t.Errorf("no MCP tools expected: %s", names)
	}
}

// The api loop's MCP servers keep hrn's environment, which their tools may
// need, but under the guard they are told so: an hrn among them then refuses
// to lift the guard.
func TestAPILoopTellsItsMCPServersTheRunIsGuarded(t *testing.T) {
	for _, c := range []struct {
		allow bool
		want  string
	}{{false, "[HRN_GUARDED=1]"}, {true, "[HRN_GUARDED=]"}} {
		_, _, fake := runFake(t, [][]string{
			toolTurn("toolu_g", "mcp__fake__echo", `{"text":"x"}`),
			textTurn("done"),
		}, Task{Prompt: "p", Dir: t.TempDir(), Options: Options{Mode: ModeAsk, AllowDangerous: c.allow, MCP: []string{mcpConfigFile(t, os.Args[0])}}})
		if block := lastToolResult(t, fake.requests[1]); !strings.Contains(fmt.Sprint(block["content"]), c.want) {
			t.Errorf("allow_dangerous %v: the server answered %v, want %s", c.allow, block["content"], c.want)
		}
	}
}
