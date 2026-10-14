package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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
		{"run without requested model", "", "variant", false},
		{"run without requested model and different reported model", "", "other", true},
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
			var reportedModels []string
			err = resolveRetrievalRunsWithModels(m, cells, &reportedModels)
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
				if got := cells[0].Usage.Attempts; len(got) != 1 || got[0].Model != v.Model {
					t.Fatalf("accepted attempts = %+v", got)
				}
				if len(reportedModels) != 1 || reportedModels[0] != tc.reported {
					t.Fatalf("reported models = %v", reportedModels)
				}
				var out bytes.Buffer
				if err := writeRetrievalModels(&out, m, reportedModels); err != nil {
					t.Fatal(err)
				}
				if out.String() != "Models: "+want+"\n" {
					t.Fatalf("model output = %q; want %q", out.String(), want)
				}
			}
		})
	}
}

func TestRetrievalCompareResolvedAliasCommand(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	m, cells, err := eval.RetrievalFixture()
	if err != nil {
		t.Fatal(err)
	}
	m.EvidenceKind = "measured"
	for i := range m.Spec.Matrix {
		m.Spec.Matrix[i].Model = "variant"
	}
	m.Spec.Matrix[1].RetrievalVersion = "version"
	v := m.Spec.Matrix[0]
	task := m.Spec.Tasks[0]
	w, err := runlog.Create(v.Backend, agent.Task{Prompt: task.Prompt, Options: agent.Options{Model: v.Model, Effort: v.Effort, RetrievalRevision: m.Revision}})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{Backend: v.Backend, Model: "variant-20261001", Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	var cell eval.RetrievalSample
	for _, sample := range cells {
		if sample.TaskID == task.ID && sample.Variant == v.Name {
			cell = sample
			break
		}
	}
	cell.RunRefs = []string{w.ID()}
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.json")
	samplesPath := filepath.Join(dir, "samples.json")
	for _, file := range []struct {
		path  string
		value any
	}{{manifestPath, m}, {samplesPath, []eval.RetrievalSample{cell}}} {
		data, err := json.Marshal(file.value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file.path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := runRetrieval([]string{"compare", "--manifest", manifestPath, "--samples", samplesPath}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Models: variant (reported variant-20261001)\n") {
		t.Fatalf("comparison output = %q", out.String())
	}
}
