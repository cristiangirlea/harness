package resolve

import (
	"encoding/json"
	"github.com/cristiangirlea/harness/internal/agent"
	"strings"
	"testing"
)

func TestDefaultClaudeTurnCap(t *testing.T) {
	for _, tc := range []struct {
		name, backend, purpose, prompt string
		given, want                    int
		applied                        bool
	}{
		{"review", "claude", "review", "work", 0, 30, true},
		{"test", "claude", "test", "work", 0, 30, true},
		{"classified", "claude", "", "Review the diff", 0, 30, true},
		{"implement", "claude", "implement", "Review the diff", 0, 0, false},
		{"explicit", "claude", "review", "work", 12, 12, false},
		{"unlimited", "claude", "review", "work", -1, -1, false},
		{"codex", "codex", "review", "work", 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := agent.Task{Purpose: tc.purpose, Prompt: tc.prompt, Options: agent.Options{MaxTurns: tc.given}}
			if got := ApplyTurnDefault(&task, tc.backend); got != tc.applied || task.MaxTurns != tc.want {
				t.Fatalf("applied=%v max_turns=%d, want %v %d", got, task.MaxTurns, tc.applied, tc.want)
			}
			data, err := json.Marshal(task)
			if err != nil {
				t.Fatal(err)
			}
			if tc.applied && !strings.Contains(string(data), `"max_turns":30`) {
				t.Fatalf("record: %s", data)
			}
		})
	}
}
