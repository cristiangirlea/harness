package platform

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
)

func TestValidateSubmissionRefusesUnsupportedDurableOptions(t *testing.T) {
	tests := []struct {
		name       string
		options    string
		wantOption string
	}{
		{name: "mcp server", options: `{"mcp":["fixture"]}`, wantOption: "mcp"},
		{name: "allow dangerous", options: `{"allow_dangerous":true}`, wantOption: "allow_dangerous"},
		{name: "both", options: `{"mcp":["fixture"],"allow_dangerous":true}`, wantOption: "mcp"},
		{name: "retrieval", options: `{"retrieval":"auto"}`, wantOption: "retrieval"},
		{name: "untrusted retrieval", options: `{"retrieval":"untrusted-value\n<script>"}`, wantOption: "retrieval"},
		{name: "retrieval off", options: `{"retrieval":"off"}`},
		{name: "empty mcp and safe", options: `{"mcp":[],"allow_dangerous":false}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := Submission{
				Prompt:  "inspect",
				Options: agent.Options{Mode: agent.ModeReadOnly, MaxCostUSD: 1, MaxTurns: 3},
				Backend: "api",
				BaseRef: strings.Repeat("a", 40),
			}
			if err := json.Unmarshal([]byte(tt.options), &s.Options); err != nil {
				t.Fatal(err)
			}
			err := validateSubmission(s)
			if tt.wantOption == "" {
				if err != nil {
					t.Fatalf("safe submission: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), tt.wantOption) {
				t.Fatalf("submission with %s: %v, want ErrUnsupported naming option", tt.wantOption, err)
			}
			if tt.wantOption == "retrieval" {
				want := "unsupported remote execution mode: retrieval is not supported for durable jobs"
				if err.Error() != want {
					t.Fatalf("retrieval refusal = %q, want %q", err.Error(), want)
				}
			}
		})
	}
}
