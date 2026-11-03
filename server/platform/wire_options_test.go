package platform

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestJobOptionsValidation(t *testing.T) {
	cases := []struct {
		name  string
		value JobOptions
	}{
		{"mode", JobOptions{Mode: "invalid"}},
		{"effort", JobOptions{Effort: "invalid"}},
		{"prompt size", JobOptions{AppendSystemPrompt: strings.Repeat("x", 16385)}},
		{"prompt NUL", JobOptions{AppendSystemPrompt: "\x00"}},
		{"compact window", JobOptions{CompactWindow: "99999"}},
		{"negative timeout", JobOptions{Timeout: -1}},
		{"negative cost", JobOptions{MaxCostUSD: -1}},
		{"nan cost", JobOptions{MaxCostUSD: math.NaN()}},
		{"infinite cost", JobOptions{MaxCostUSD: math.Inf(1)}},
		{"escaped prompt size", JobOptions{AppendSystemPrompt: strings.Repeat("\\", 8193)}},
		{"task budget", JobOptions{TaskBudget: 19999}},
		{"retrieval", JobOptions{Retrieval: "unknown"}},
		{"retrieval fields", JobOptions{Retrieval: "required"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.value.Validate() == nil {
				t.Fatal("expected error")
			}
		})
	}
	if err := (JobOptions{Mode: "readonly", TaskBudget: 20000}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestJobOptionsRejectsUnknownFields(t *testing.T) {
	var o JobOptions
	if err := json.Unmarshal([]byte(`{"unknown":true}`), &o); err == nil {
		t.Fatal("expected error")
	}
}

func TestJobOptionsWireValues(t *testing.T) {
	for _, input := range []string{`{"timeout":"20m","compact_window":100000}`, `{"timeout":null,"compact_window":"auto"}`} {
		var o JobOptions
		if err := json.Unmarshal([]byte(input), &o); err != nil {
			t.Fatal(err)
		}
		if err := o.Validate(); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		var again JobOptions
		if err := json.Unmarshal(encoded, &again); err != nil {
			t.Fatal(err)
		}
		if o.Timeout != again.Timeout || o.CompactWindow != again.CompactWindow {
			t.Fatalf("wire values changed: %s", encoded)
		}
	}
	for _, input := range []string{`{"timeout":123}`, `{"compact_window":"01"}`} {
		var o JobOptions
		if err := json.Unmarshal([]byte(input), &o); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}
