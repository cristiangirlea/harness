package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/retrieval"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLessonComparisonRejectsUnsafeInputs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Spec)
		held   float64
		want   string
	}{
		{"matrix", func(s *Spec) { s.Matrix = nil }, 0, "one variant"},
		{"backend", func(s *Spec) { s.Matrix[0].Backend = "api" }, 0, "plan backend"},
		{"repeats", func(s *Spec) { s.Repeats = 1 }, 0, "at least 2"},
		{"split", func(s *Spec) {}, 2, "between 0 and 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testSpec("unused")
			s.Matrix = []Variant{{Backend: "ev-setup"}}
			tc.change(&s)
			_, err := RunLessonComparison(context.Background(), s, "active", "candidate", tc.held, 0, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %s", err, tc.want)
			}
		})
	}
}

type lessonStub struct{ stub }

func (s lessonStub) Run(_ context.Context, task agent.Task, emit func(agent.Event)) agent.Result {
	final := "fail"
	if strings.Contains(task.Prompt, "candidate lesson") {
		final = "pass"
	}
	return agent.Result{Backend: s.name, Status: agent.StatusOK, Final: final, Turns: 2, InputTokens: 100, Usage: &agent.UsageMetadata{InputSemantics: "exclusive_cache", Status: "reported"}}
}
func TestLessonComparisonPinsBothRetrievalVersions(t *testing.T) {
	dir := initRepo(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q retrieval.Search
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
			return
		}
		content := "baseline lesson"
		if q.VersionID == "candidate" {
			content = "candidate lesson"
		}
		entry := retrieval.Entry{ID: "fixture", Content: content}
		text := fmt.Sprintf("[Reference fixture; source=\"\"; revision=%s; evidence=\"\"]\n%s\n[/Reference]\n", q.Revision, content)
		json.NewEncoder(w).Encode(retrieval.Packet{VersionID: q.VersionID, Entries: []retrieval.Entry{entry}, Text: text, Bytes: len(text), Digest: retrieval.TextDigest(text), QueryDigest: retrieval.TextDigest(q.Query)})
	}))
	defer srv.Close()
	t.Setenv("HRN_SERVER_URL", srv.URL)
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("t", 64))
	t.Setenv("HRN_SERVER_CA_FILE", "")
	agent.Register(lessonStub{stub{"ev-lesson", false}})
	setupBackends = append(setupBackends, "ev-lesson")
	t.Cleanup(func() { setupBackends = setupBackends[:len(setupBackends)-1] })
	spec := testSpec(dir)
	spec.Gate = nil
	spec.Tasks = []batch.Task{{ID: "one", Prompt: "test", Expect: "^pass$"}}
	spec.Matrix = []Variant{{Backend: "ev-lesson", RetrievalVersion: "wrong"}}
	spec.Tasks[0].RetrievalVersion = "wrong"
	spec.Options = agent.Options{Mode: agent.ModeReadOnly, Model: "fixture", Retrieval: "required", RetrievalTenant: "t_fixture", RetrievalProject: "p_fixture", RetrievalRevision: strings.Repeat("a", 40)}
	run, err := RunLessonComparison(context.Background(), spec, "baseline", "candidate", 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if run.Comparison.Baseline.PassRate != 0 || run.Comparison.Candidate.PassRate != 1 || run.Comparison.Baseline.Eval != run.Baseline.ID || run.Comparison.Candidate.Eval != run.Candidate.ID {
		t.Fatalf("comparison: %+v", run.Comparison)
	}
}
