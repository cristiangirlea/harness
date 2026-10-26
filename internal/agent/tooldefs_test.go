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

// toolDefinitionBudget is each mode's measured size rounded up to 512 bytes.
var toolDefinitionBudget = map[string]int{
	"readonly":          1536,
	"auto":              3072,
	"yolo":              3072,
	"yolo with shaping": 3584,
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
