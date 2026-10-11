package agent

import (
	"slices"
	"testing"
)

func TestCodexPassesEffortBeforeForkAndPrompt(t *testing.T) {
	for _, fork := range []string{"", "session-123"} {
		for _, effort := range []string{"", "high"} {
			t.Run("fork="+fork+"/effort="+effort, func(t *testing.T) {
				args := NewCodexBackend().args(Task{Dir: "/workspace", ForkSession: fork, Options: Options{Mode: ModeAuto, Effort: effort}}, "")
				config := slices.Index(args, "-c")
				if effort == "" {
					if config != -1 {
						t.Fatalf("unset effort must keep the CLI default: %v", args)
					}
				} else if config < 1 || config+1 >= len(args) || args[config+1] != "model_reasoning_effort=high" {
					t.Fatalf("want -c model_reasoning_effort=high among exec options: %v", args)
				}
				if args[0] != "exec" || args[len(args)-1] != "-" {
					t.Fatalf("want exec with a final stdin prompt: %v", args)
				}
				if effort != "" && config+1 >= len(args)-1 {
					t.Errorf("effort must precede the final -: %v", args)
				}
				if fork != "" {
					forkIndex := slices.Index(args, "fork")
					if forkIndex < 0 || forkIndex+1 >= len(args) || args[forkIndex+1] != fork {
						t.Fatalf("want fork %s: %v", fork, args)
					}
					if effort != "" && config+1 >= forkIndex {
						t.Errorf("effort must precede fork: %v", args)
					}
				}
			})
		}
	}
}
