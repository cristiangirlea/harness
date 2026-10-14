package hrnmcp

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/cristiangirlea/harness/internal/mcp"
	"github.com/cristiangirlea/harness/internal/prefs"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestRunToolOffersOnlyEnabledBackendsAndRefusesDisabledOnes(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: []string{"codex", "claude"}}); err != nil {
		t.Fatal(err)
	}
	tool := runTool(t.TempDir(), false)
	props := tool.InputSchema["properties"].(map[string]any)
	if got := props["backend"].(map[string]any)["enum"]; !reflect.DeepEqual(got, []any{"codex", "claude"}) {
		t.Errorf("enum: %v", got)
	}
	// An unknown name cannot launch a real backend even before the refusal exists.
	result, err := tool.Handler(context.Background(), mcp.Call{Arguments: json.RawMessage(`{"prompt":"hello","backend":"disabled-test"}`)})
	want := "run: backend disabled-test is disabled; enabled: codex, claude (hrn backends set)"
	if err != nil || !result.IsError || len(result.Content) == 0 || result.Content[0].Text != want {
		t.Fatalf("got %+v, %v", result, err)
	}
}

func TestBackendsToolShowsEnabledAndDefaultPreferences(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: []string{"claude"}, DefaultName: "claude"}); err != nil {
		t.Fatal(err)
	}
	result, err := backendsTool().Handler(context.Background(), mcp.Call{})
	if err != nil {
		t.Fatal(err)
	}
	data := result.StructuredContent.(map[string]any)
	for _, entry := range data["backends"].([]map[string]any) {
		want := entry["name"] == "claude"
		if entry["enabled"] != want || entry["default"] != want {
			t.Errorf("entry: %v", entry)
		}
	}
	if data["default"] != "claude" {
		t.Errorf("default: %v", data["default"])
	}
}
