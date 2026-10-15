package batch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/purpose"
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
			if err == nil || !strings.Contains(err.Error(), "one of "+strings.Join(purpose.Declarable(), ", ")) {
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

func TestAnOldBatchResultWithoutAPurposeStillLoads(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	if err := os.MkdirAll(batchesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	const id = "20260101-000000-abcdef"
	old := `{"id":"` + id + `","tasks":[{"id":"t1","prompt":"Fix it.","status":"ok","branch":"hrn/t1"}]}`
	if err := os.WriteFile(filepath.Join(batchesDir(), id+".json"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := LoadResult(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tasks) != 1 || res.Tasks[0].Purpose != "" || res.Tasks[0].Prompt != "Fix it." {
		t.Fatalf("tasks = %+v, want one task with no purpose", res.Tasks)
	}
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"purpose"`) {
		t.Errorf("the re-encoded result names a purpose it never had: %s", data)
	}
}
