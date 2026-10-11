package agent

import (
	"slices"
	"testing"
)

func TestClaudePassesEffortOnlyWhenSet(t *testing.T) {
	for _, effort := range []string{"", "low", "medium", "high", "xhigh", "max"} {
		t.Run("effort="+effort, func(t *testing.T) {
			args := NewClaudeBackend().args(Task{Options: Options{Effort: effort}}, "")
			index := slices.Index(args, "--effort")
			if effort == "" {
				if index != -1 {
					t.Errorf("unset effort must keep the CLI default: %v", args)
				}
				return
			}
			if index < 0 || index+1 >= len(args) || args[index+1] != effort {
				t.Errorf("want --effort %s: %v", effort, args)
			}
		})
	}
}
