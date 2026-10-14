package eval

import (
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/prefs"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestEvalRefusesADisabledJudgeBeforeRunningCells(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	j := Judge{Backend: "codex", Rubric: "correct"}
	if err := j.check([]batch.Task{{ID: "one", Prompt: "hello"}}); err == nil || !strings.Contains(err.Error(), "backend codex is disabled") {
		t.Fatalf("got %v", err)
	}
}
