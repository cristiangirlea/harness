package agent

import (
	"encoding/json"
	"reflect"
	"testing"
)

var toolDefinitionModes = []struct {
	name string
	opts Options
}{
	{"readonly", Options{Mode: ModeReadOnly}},
	{"auto", Options{Mode: ModeAuto}},
	{"yolo", Options{Mode: ModeYolo}},
	{"yolo with shaping", Options{Mode: ModeYolo, ShapeTools: true}},
}

func TestToolDefinitionsAreTheToolsTheRequestCarries(t *testing.T) {
	for _, m := range toolDefinitionModes {
		t.Run(m.name, func(t *testing.T) {
			task := Task{Prompt: "look", Dir: t.TempDir(), Options: m.opts}
			_, _, fake := runFake(t, [][]string{textTurn("ok")}, task)
			defs, err := NewAPIBackend().ToolDefinitions(task)
			if err != nil {
				t.Fatal(err)
			}
			var got any
			if err := json.Unmarshal(defs, &got); err != nil {
				t.Fatal(err)
			}
			if want := fake.requests[0]["tools"]; !reflect.DeepEqual(got, want) {
				t.Fatalf("definitions differ from the request's tools field:\n got %s", defs)
			}
		})
	}
}

// toolDefinitionBudget leaves about 5% headroom over each measured size.
var toolDefinitionBudget = map[string]int{
	"readonly":          1228,
	"auto":              2382,
	"yolo":              2382,
	"yolo with shaping": 2932,
}

func TestToolDefinitionsStayUnderTheirByteBudget(t *testing.T) {
	for _, m := range toolDefinitionModes {
		defs, err := NewAPIBackend().ToolDefinitions(Task{Options: m.opts})
		if err != nil {
			t.Fatal(err)
		}
		if budget := toolDefinitionBudget[m.name]; len(defs) > budget {
			t.Errorf("%s: tool definitions are %d bytes, over the %d-byte budget; measure again with bash scripts/tool-shapes.sh --definitions and update docs/tool-shapes.md before raising it",
				m.name, len(defs), budget)
		}
	}
}

func TestToolDefinitionsModeNarrowing(t *testing.T) {
	names := func(opts Options) map[string]bool {
		t.Helper()
		defs, err := NewAPIBackend().ToolDefinitions(Task{Options: opts})
		if err != nil {
			t.Fatal(err)
		}
		var tools []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(defs, &tools); err != nil {
			t.Fatal(err)
		}
		result := make(map[string]bool)
		for _, tool := range tools {
			if result[tool.Name] {
				t.Errorf("duplicate tool %q", tool.Name)
			}
			result[tool.Name] = true
		}
		return result
	}
	readonly := names(Options{Mode: ModeReadOnly})
	want := map[string]bool{"read_file": true, "list_files": true, "grep": true}
	if !reflect.DeepEqual(readonly, want) {
		t.Errorf("readonly tools = %v, want %v", readonly, want)
	}
	for _, mode := range []Mode{ModeReadOnly, ModeAuto, ModeYolo} {
		base := names(Options{Mode: mode})
		if base["read_original"] {
			t.Errorf("%s: read_original offered without shaping", mode)
		}
		shaped := names(Options{Mode: mode, ShapeTools: true})
		base["read_original"] = true
		if !reflect.DeepEqual(shaped, base) {
			t.Errorf("%s: shaping tools = %v, want %v", mode, shaped, base)
		}
	}
}
