package hrnmcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/runlog"
)

const purposeList = "review, debug, implement, refactor, test, research, plan, ops"

func TestRunPurposeIsValidatedAndStored(t *testing.T) {
	s := connect(t, t.TempDir())
	for _, value := range []string{"x", "unknown", "Review"} {
		res := s.call(t, "run", `{"prompt":"fix it","backend":"stub","purpose":"`+value+`"}`)
		if !res.IsError || !strings.Contains(res.Text(), purposeList) {
			t.Errorf("invalid purpose %q: %s", value, res.Text())
		}
	}
	if recs, err := runlog.List(0); err != nil || len(recs) != 0 {
		t.Errorf("invalid purpose left records: %v, %v", recs, err)
	}
	for _, tc := range []struct{ declared, prompt, want string }{
		{"plan", "Purpose: review\nFix it.", "plan"},
		{"", "Purpose: ReViEw\nFix it.", "review"},
		{"", "Purpose: free text\nFix it.", ""},
		{"", "Purpose: unknown\nFix it.", ""},
		{"", "Fix it.", ""},
	} {
		args, _ := json.Marshal(map[string]string{"prompt": tc.prompt, "purpose": tc.declared, "backend": "stub"})
		res := s.call(t, "run", string(args))
		if res.IsError {
			t.Errorf("run: %s", res.Text())
			continue
		}
		id := res.StructuredContent.(map[string]any)["run_id"].(string)
		rec, err := runlog.Load(id)
		if err != nil || rec.Task.Purpose != tc.want || rec.Task.Prompt != tc.prompt || rec.Result.Final != "stub says: "+tc.prompt {
			t.Errorf("record=%+v, error=%v; want purpose %q and unchanged prompt", rec, err, tc.want)
		}
	}
}

func TestMCPDescriptionsOfferPurposeCategories(t *testing.T) {
	s := connect(t, t.TempDir())
	for _, tool := range s.cl.Tools() {
		if tool.Name == "run" {
			var schema struct {
				Properties map[string]struct {
					Description string `json:"description"`
				} `json:"properties"`
			}
			if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(schema.Properties["purpose"].Description, purposeList) {
				t.Errorf("run purpose schema = %+v", schema.Properties["purpose"])
			}
		}
	}
	if !strings.Contains(specDescription, "purpose") || !strings.Contains(specDescription, purposeList) {
		t.Errorf("batch spec description lacks purpose categories: %s", specDescription)
	}
}

func TestBatchPurposeIsValidatedAndStored(t *testing.T) {
	s := connect(t, gitRepo(t))
	bad := s.call(t, "batch", `{"spec":{"backend":"stub","tasks":[{"prompt":"Fix it.","purpose":"unknown"}]}}`)
	if !bad.IsError || !strings.Contains(bad.Text(), purposeList) {
		t.Errorf("invalid batch purpose: %s", bad.Text())
	}
	res := s.call(t, "batch", `{"spec":{"backend":"stub","parallel":1,"worktrees":"fresh","tasks":[{"id":"explicit","prompt":"Purpose: review\nFix it.","purpose":"plan"},{"id":"header","prompt":"Purpose: ReViEw\nFix it."}]}}`)
	if res.IsError {
		t.Fatalf("batch: %s", res.Text())
	}
	data, _ := json.Marshal(res.StructuredContent)
	var brief struct {
		Tasks []struct {
			Purpose string `json:"purpose"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &brief); err != nil || len(brief.Tasks) != 2 {
		t.Fatalf("batch brief=%s error=%v", data, err)
	}
	if brief.Tasks[0].Purpose != "plan" || brief.Tasks[1].Purpose != "review" {
		t.Errorf("batch brief lacks declarations: %s", data)
	}
	recs, err := runlog.List(0)
	if err != nil || len(recs) != 2 {
		t.Fatalf("records=%v error=%v", recs, err)
	}
	for _, rec := range recs {
		want := "plan"
		if strings.Contains(rec.Task.Prompt, "ReViEw") {
			want = "review"
		}
		if rec.Task.Purpose != want || rec.Result.Final != "stub says: "+rec.Task.Prompt {
			t.Errorf("record=%+v, want purpose %q and unchanged prompt", rec, want)
		}
	}
}
