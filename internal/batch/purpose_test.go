package batch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSpecValidatesPurposeBeforeAnyTaskRuns(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "spec.json")
	for _, value := range []string{"x", "unknown", "Review", "plan", ""} {
		data := `{"backend":"wb-noop","tasks":[{"prompt":"Purpose: ReViEw\nFix it.","purpose":"` + value + `"}]}`
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		spec, err := LoadSpec(path)
		if value != "plan" && value != "" {
			if err == nil || !strings.Contains(err.Error(), "one of review, debug, implement, refactor, test, research, plan, ops") {
				t.Errorf("purpose %q: error=%v", value, err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		// Running the parsed spec must preserve the declaration even if attempts
		// later add retry context to the prompt.
		spec.Dir = initRepo(t)
		spec.Worktrees = WorktreesFresh
		res, _ := run(t, spec)
		rec := firstRun(t, res.Tasks[0])
		want := value
		if want == "" {
			want = "review"
		}
		if rec.Task.Purpose != want || rec.Task.Prompt != "Purpose: ReViEw\nFix it." {
			t.Errorf("task=%+v, want purpose %q and unchanged prompt", rec.Task, want)
		}
	}
}
