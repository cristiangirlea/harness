package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/eval"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestRetrievalFixtureCommand(t *testing.T) {
	t.Setenv("HRN_SERVER_URL", "invalid")
	var out bytes.Buffer
	if e := runRetrieval([]string{"fixture", "--json"}, &out); e != nil {
		t.Fatal(e)
	}
	var c eval.RetrievalComparison
	if e := json.Unmarshal(out.Bytes(), &c); e != nil {
		t.Fatal(e)
	}
	if c.ObservedPairs != 30 || c.EvidenceKind != "synthetic_fixture" || c.TokenSavingsSupported || c.CostSavingsSupported {
		t.Fatalf("%+v", c)
	}
	out.Reset()
	if e := runRetrieval([]string{"fixture"}, &out); e != nil || !strings.Contains(out.String(), "synthetic_fixture") {
		t.Fatal(e, out.String())
	}
	t.Log(out.String())
}

func TestRetrievalCompareRunModelIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, requested, reported string
		wantError                 bool
	}{
		{"resolved alias", "variant", "variant-20261001", false},
		{"different requested model", "other", "variant", true},
		{"old record without requested model", "", "variant", false},
		{"old record with different reported model", "", "other", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			m, _, err := eval.RetrievalFixture()
			if err != nil {
				t.Fatal(err)
			}
			for i := range m.Spec.Matrix {
				m.Spec.Matrix[i].Model = "variant"
			}
			v := &m.Spec.Matrix[0]
			v.Retrieval = "off"
			task := m.Spec.Tasks[0]
			cell := eval.RetrievalSample{TaskID: task.ID, Variant: v.Name, RunRefs: nil}
			w, err := runlog.Create(v.Backend, agent.Task{Prompt: task.Prompt, Options: agent.Options{Model: tc.requested, Effort: v.Effort, RetrievalRevision: m.Revision}})
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Close(agent.Result{Backend: v.Backend, Model: tc.reported, Status: agent.StatusOK}); err != nil {
				t.Fatal(err)
			}
			cell.RunRefs = []string{w.ID()}
			cells := []eval.RetrievalSample{cell}
			err = resolveRetrievalRuns(m, cells)
			if (err != nil) != tc.wantError {
				t.Fatalf("resolve error = %v; wantError = %t", err, tc.wantError)
			}
			if tc.wantError && !strings.Contains(err.Error(), "backend/model/effort differs") {
				t.Fatalf("unexpected rejection: %v", err)
			}
			if !tc.wantError {
				want := "variant"
				if tc.reported != want {
					want += " (reported " + tc.reported + ")"
				}
				if got := cells[0].Usage.Attempts; len(got) != 1 || got[0].Model != tc.reported {
					t.Fatalf("accepted attempts = %+v", got)
				}
				var out bytes.Buffer
				if err := writeRetrievalModels(&out, m, cells); err != nil {
					t.Fatal(err)
				}
				if out.String() != "Models: "+want+"\n" {
					t.Fatalf("model output = %q; want %q", out.String(), want)
				}
			}
		})
	}
}
