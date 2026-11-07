package eval

import (
	"encoding/json"
	"testing"

	"github.com/cristiangirlea/harness/internal/batch"
)

func TestVariantCountsRefusalFlagsAcrossAllAttempts(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	spec := Spec{Spec: batch.Spec{Tasks: []batch.Task{{ID: "task"}}}, Matrix: []Variant{{Backend: "stub"}}}
	var res batch.Result
	if err := json.Unmarshal([]byte(`{"tasks":[{"id":"e.task.stub.r1","status":"pass","attempts":[{"result":{"status":"error","refusal_flags":[{"kind":"written"}]}},{"result":{"status":"ok","turns":2,"refusal_flags":[{"kind":"script"},{"kind":"wrapped"}]}}]}]}`), &res); err != nil {
		t.Fatal(err)
	}
	table := Summarize(spec, res, nil)
	if got := table.Variants[0].RefusalFlags; got != 3 {
		t.Fatalf("flags = %d", got)
	}
}
