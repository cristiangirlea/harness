package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/eval"
	"github.com/cristiangirlea/harness/internal/report"
	"github.com/cristiangirlea/harness/internal/retrieval"
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
			w, err := runlog.Create(v.Backend, agent.Task{Prompt: task.Prompt, Options: agent.Options{Model: tc.requested, Effort: v.Effort, RetrievalRevision: m.Revision}}, nil)
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
	w, err := runlog.Create(v.Backend, agent.Task{Prompt: task.Prompt, Options: agent.Options{Model: v.Model, Effort: v.Effort, RetrievalRevision: m.Revision}}, nil)
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

func writeRetrievalFixtureJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func retrievalSamplesEval(t *testing.T, m eval.RetrievalManifest, id string, repeats int, swapped bool) eval.Record {
	t.Helper()
	spec := m.Spec
	spec.Repeats = repeats
	spec.Matrix = append([]eval.Variant(nil), spec.Matrix...)
	if swapped {
		spec.Matrix[0], spec.Matrix[1] = spec.Matrix[1], spec.Matrix[0]
	}
	bs, err := eval.Expand(spec, id)
	if err != nil {
		t.Fatal(err)
	}
	rec := eval.Record{ID: id, BatchID: id + "-batch", Spec: spec}
	for _, task := range bs.Tasks {
		opts := agent.Options{Model: task.Model, Effort: task.Effort, RetrievalRevision: m.Revision, Retrieval: task.Retrieval}
		var pin *retrieval.Record
		if task.Retrieval != "off" {
			opts.RetrievalTenant, opts.RetrievalProject = "tenant", "project"
			opts.RetrievalVersion = task.RetrievalVersion
			pin = &retrieval.Record{Mode: task.Retrieval, OriginalPrompt: task.Prompt, DurationNS: 17,
				Scope:  retrieval.Scope{TenantID: "tenant", ProjectID: "project", Backend: task.Backend, Model: task.Model, Revision: m.Revision},
				Packet: &retrieval.Packet{VersionID: task.RetrievalVersion, Digest: retrieval.TextDigest("")}}
		}
		w, err := runlog.Create(task.Backend, agent.Task{Prompt: task.Prompt, Options: opts}, nil)
		if err != nil {
			t.Fatal(err)
		}
		result := agent.Result{Backend: task.Backend, Model: task.Model, Status: agent.StatusOK, Retrieval: pin,
			InputTokens: 100, Usage: &agent.UsageMetadata{InputSemantics: "exclusive_cache", Status: "reported"}}
		if err := w.Close(result); err != nil {
			t.Fatal(err)
		}
		if task.Retrieval != "off" {
			writeRetrievalFixtureJSON(t, filepath.Join(runlog.Root(), w.ID(), "retrieval.json"), pin)
		}
		rec.Results = append(rec.Results, batch.TaskResult{ID: task.ID, Status: batch.StatusPass, Attempts: []batch.Attempt{{RunID: w.ID(), Result: result}}})
	}
	return rec
}

func samplesCommand(t *testing.T, m eval.RetrievalManifest, records []eval.Record, refs ...string) ([]eval.RetrievalSample, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.json")
	writeRetrievalFixtureJSON(t, path, m)
	for _, rec := range records {
		writeRetrievalEvalBatches(t, m, rec)
		writeRetrievalFixtureJSON(t, filepath.Join(runlog.Home(), "evals", rec.ID+".json"), rec)
	}
	args := []string{"samples", "--manifest", path}
	for _, ref := range refs {
		args = append(args, "--eval", ref)
	}
	var out bytes.Buffer
	if err := runRetrieval(args, &out); err != nil {
		return nil, err
	}
	var samples []eval.RetrievalSample
	if err := json.Unmarshal(out.Bytes(), &samples); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "[\n  {") {
		t.Fatalf("output not indented: %s", &out)
	}
	return samples, nil
}

func TestRetrievalSamplesCommand(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	m, _, err := eval.RetrievalFixture()
	if err != nil {
		t.Fatal(err)
	}
	a := retrievalSamplesEval(t, m, "first", 1, false)
	b := retrievalSamplesEval(t, m, "second", 2, true)
	a.Results[0].Commit = "changed"
	var retried []string
	for i := range a.Results {
		if strings.Contains(a.Results[i].ID, "."+m.Spec.Matrix[1].Key()+".") {
			attempt := a.Results[i].Attempts[0]
			w, err := runlog.Create(attempt.Result.Backend, agent.Task{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Close(attempt.Result); err != nil {
				t.Fatal(err)
			}
			writeRetrievalFixtureJSON(t, filepath.Join(runlog.Root(), w.ID(), "retrieval.json"), retrieval.Record{DurationNS: 17})
			retried = []string{attempt.RunID, w.ID()}
			attempt.RunID = w.ID()
			a.Results[i].Attempts = append(a.Results[i].Attempts, attempt)
			break
		}
	}
	samples, err := samplesCommand(t, m, []eval.Record{a, b}, "first", "second:1")
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 60 || !samples[0].IsolationFailure {
		t.Fatalf("samples = %+v", samples)
	}
	for i := range samples {
		s := &samples[i]
		if s.Usage.Attempts == nil || len(s.Usage.Attempts) != 0 {
			t.Fatalf("attempts = %+v", s.Usage.Attempts)
		}
		if s.Usage.Online.ModelTokens == nil || *s.Usage.Online.ModelTokens != 0 || s.Usage.Online.CostUSD == nil || *s.Usage.Online.CostUSD != 0 {
			t.Fatalf("online = %+v", s.Usage.Online)
		}
		wantDuration := int64(0)
		if s.Variant == m.Spec.Matrix[1].Key() {
			wantDuration = 17 * int64(len(s.RunRefs))
		}
		if s.Usage.Online.DurationNS != wantDuration {
			t.Fatalf("duration = %+v; want %d", s, wantDuration)
		}
		if len(s.RunRefs) == 2 && strings.Join(s.RunRefs, ",") != strings.Join(retried, ",") {
			t.Fatalf("attempt order = %v; want %v", s.RunRefs, retried)
		}
		for _, id := range s.RunRefs {
			r, err := runlog.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			s.Usage.Attempts = append(s.Usage.Attempts, r.Result)
		}
	}
	if _, err := eval.CompareRetrieval(m, samples, report.Overhead{}, report.Overhead{}, 1); err != nil {
		t.Fatal(err)
	}
}

func TestRetrievalSamplesRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*eval.Record)
		offset string
	}{
		{"prefix", func(r *eval.Record) { r.Results[0].ID = "wrong.t1.baseline.r1" }, ""},
		{"malformed", func(r *eval.Record) { r.Results[0].ID = "broken" }, ""},
		{"unknown task", func(r *eval.Record) {
			p := strings.Split(r.Results[0].ID, ".")
			p[1] = "unknown"
			r.Results[0].ID = strings.Join(p, ".")
		}, ""},
		{"unknown variant", func(r *eval.Record) {
			p := strings.Split(r.Results[0].ID, ".")
			p[2] = "unknown"
			r.Results[0].ID = strings.Join(p, ".")
		}, ""},
		{"repeat zero", func(r *eval.Record) { r.Results[0].ID = strings.TrimSuffix(r.Results[0].ID, "r1") + "r0" }, ""},
		{"repeat overflow", func(r *eval.Record) {}, ":3"},
		{"negative offset", func(r *eval.Record) {}, ":-1"},
		{"duplicate", func(r *eval.Record) { r.Results = append(r.Results, r.Results[0]) }, ""},
		{"running", func(r *eval.Record) { r.Results[0].Status = batch.StatusRunning }, ""},
		{"empty status", func(r *eval.Record) { r.Results[0].Status = "" }, ""},
		{"unknown status", func(r *eval.Record) { r.Results[0].Status = "unknown" }, ""},
		{"empty run", func(r *eval.Record) { r.Results[0].Attempts[0].RunID = "" }, ""},
		{"no attempts", func(r *eval.Record) { r.Results[0].Status = batch.StatusCanceled; r.Results[0].Attempts = nil }, ""},
		{"missing cell", func(r *eval.Record) { r.Results = r.Results[1:] }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			m, _, err := eval.RetrievalFixture()
			if err != nil {
				t.Fatal(err)
			}
			r := retrievalSamplesEval(t, m, "test", m.Spec.Repeats, false)
			missing := r.Results[0].ID
			tc.mutate(&r)
			cell := r.Results[0].ID
			if tc.name == "missing cell" {
				cell = strings.TrimPrefix(missing, "test.")
			}
			_, err = samplesCommand(t, m, []eval.Record{r}, "test"+tc.offset)
			if err == nil || !strings.Contains(err.Error(), cell) {
				t.Fatalf("error = %v; want cell %s", err, cell)
			}
			if tc.name == "no attempts" && err.Error() != "cell "+cell+": no attempts" {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestRetrievalSamplesRetrievalFileRefusals(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			m, _, err := eval.RetrievalFixture()
			if err != nil {
				t.Fatal(err)
			}
			r := retrievalSamplesEval(t, m, "test", m.Spec.Repeats, false)
			for _, tr := range r.Results {
				if !strings.Contains(tr.ID, "."+m.Spec.Matrix[1].Key()+".") {
					continue
				}
				path := filepath.Join(runlog.Root(), tr.Attempts[0].RunID, "retrieval.json")
				if invalid {
					err = os.WriteFile(path, []byte("{"), 0600)
				} else {
					err = os.Remove(path)
				}
				if err != nil {
					t.Fatal(err)
				}
				_, err = samplesCommand(t, m, []eval.Record{r}, "test")
				if err == nil || !strings.Contains(err.Error(), tr.ID) {
					t.Fatalf("error = %v; want cell %s", err, tr.ID)
				}
				break
			}
		})
	}
}

func TestRetrievalSamplesDuplicateEvals(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	m, _, err := eval.RetrievalFixture()
	if err != nil {
		t.Fatal(err)
	}
	a := retrievalSamplesEval(t, m, "first", 1, false)
	b := retrievalSamplesEval(t, m, "second", 1, true)
	_, err = samplesCommand(t, m, []eval.Record{a, b}, "first", "second")
	if err == nil || !strings.Contains(err.Error(), b.Results[0].ID) || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate eval error = %v", err)
	}
}

func TestRetrievalSamplesManifestValidation(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	m, _, err := eval.RetrievalFixture()
	if err != nil {
		t.Fatal(err)
	}
	m.HeldOut = false
	if _, err := samplesCommand(t, m, nil, "missing"); err == nil || !strings.Contains(err.Error(), "held-out") {
		t.Fatalf("invalid manifest error = %v", err)
	}
}

func TestRetrievalSamplesTerminalFailures(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	m, _, err := eval.RetrievalFixture()
	if err != nil {
		t.Fatal(err)
	}
	r := retrievalSamplesEval(t, m, "test", m.Spec.Repeats, false)
	for i, status := range []batch.Status{batch.StatusFail, batch.StatusError, batch.StatusCanceled} {
		r.Results[i].Status = status
	}
	samples, err := samplesCommand(t, m, []eval.Record{r}, "test")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if samples[i].Usage.Passed {
			t.Fatalf("failure marked passed: %+v", samples[i])
		}
	}
}

func TestRetrievalSamplesReruns(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	m, _, err := eval.RetrievalFixture()
	if err != nil {
		t.Fatal(err)
	}
	r := retrievalSamplesEval(t, m, "test", m.Spec.Repeats, false)
	original := r.Results[0]
	r.Results[0].Status = batch.StatusError
	r.Results[0].Attempts = []batch.Attempt{{DidNotStart: "setup"}}
	rerun := original
	rerun.ID += ".u1"
	r.Results = append(r.Results, rerun)
	r.RerunBatchIDs = []string{"rerun"}
	later := rerun
	later.ID = original.ID + ".u2"
	later.Status = batch.StatusFail
	later.Attempts = nil
	r.Results = append(r.Results, later)
	samples, err := samplesCommand(t, m, []eval.Record{r}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !samples[0].Usage.Passed || samples[0].RunRefs[0] != original.Attempts[0].RunID {
		t.Fatalf("rerun = %+v", samples[0])
	}
}

func TestRetrievalCorpusDigest(t *testing.T) {
	entries := []retrieval.Entry{
		{Key: "docs/b.md > Zeta > Ünïcode", Source: "docs/b.md@abc", Content: "café <tag> & \"quote\""},
		{Key: "docs/a.md > Alpha", Source: "docs/a.md@abc", Content: "plain"},
		{Key: "docs/a.md > Alpha", Source: "docs/a0.md@abc", Content: "second source"},
	}
	for _, inspect := range []bool{false, true} {
		t.Run(fmt.Sprint(inspect), func(t *testing.T) {
			if inspect {
				for i := range entries {
					entries[i].ID = "id"
					entries[i].Evidence = "reviewed"
					entries[i].Identifiers = []string{"symbol"}
				}
				entries[0], entries[2] = entries[2], entries[0]
			}
			path := filepath.Join(t.TempDir(), "corpus.json")
			writeRetrievalFixtureJSON(t, path, retrieval.Version{ID: "version", Entries: entries})
			var out bytes.Buffer
			if err := runRetrieval([]string{"corpus-digest", path}, &out); err != nil {
				t.Fatal(err)
			}
			if out.String() != "662980aa3b76ecf079a8a7a1248608a8a26f648dee663fe4b4b01b90afe36425\n" {
				t.Fatalf("digest = %q", out.String())
			}
		})
	}
	path := filepath.Join(t.TempDir(), "empty.json")
	writeRetrievalFixtureJSON(t, path, retrieval.Version{})
	if err := runRetrieval([]string{"corpus-digest", path}, &bytes.Buffer{}); err == nil {
		t.Fatal("accepted empty corpus")
	}
}

func writeRetrievalEvalBatches(t *testing.T, m eval.RetrievalManifest, rec eval.Record) {
	t.Helper()
	for round, id := range append([]string{rec.BatchID}, rec.RerunBatchIDs...) {
		b := batch.Result{ID: id, Base: m.Revision}
		for _, tr := range rec.Results {
			parts := strings.Split(tr.ID, ".")
			if (round == 0 && len(parts) == 4) || (round > 0 && strings.HasSuffix(tr.ID, fmt.Sprintf(".u%d", round))) {
				b.Tasks = append(b.Tasks, tr)
			}
		}
		writeRetrievalFixtureJSON(t, filepath.Join(runlog.Home(), "batches", id+".json"), b)
	}
}

func TestRetrievalSamplesBatchRefusals(t *testing.T) {
	for _, kind := range []string{"missing task", "status", "run ID", "attempt count", "attempt order", "missing batch", "missing rerun"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			m, _, err := eval.RetrievalFixture()
			if err != nil {
				t.Fatal(err)
			}
			r := retrievalSamplesEval(t, m, "test", m.Spec.Repeats, false)
			r.Results[0].Attempts = append(r.Results[0].Attempts, batch.Attempt{RunID: "second"})
			writeRetrievalEvalBatches(t, m, r)
			cell := r.Results[0].ID
			switch kind {
			case "missing task":
				r.Results[0].ID = strings.Replace(cell, ".r1", ".r2", 1)
				r.Results = r.Results[:1]
				cell = r.Results[0].ID
			case "status":
				r.Results[0].Status = batch.StatusFail
			case "run ID":
				r.Results[0].Attempts[0].RunID = "different"
			case "attempt count":
				r.Results[0].Attempts = r.Results[0].Attempts[:1]
			case "attempt order":
				r.Results[0].Attempts[0], r.Results[0].Attempts[1] = r.Results[0].Attempts[1], r.Results[0].Attempts[0]
			case "missing batch":
				r.BatchID = "missing"
			case "missing rerun":
				r.RerunBatchIDs = []string{"missing"}
			}
			if kind == "missing task" {
				b, err := batch.LoadResult(r.BatchID)
				if err != nil {
					t.Fatal(err)
				}
				for i := range b.Tasks {
					if b.Tasks[i].ID == cell {
						b.Tasks = append(b.Tasks[:i], b.Tasks[i+1:]...)
						break
					}
				}
				writeRetrievalFixtureJSON(t, filepath.Join(runlog.Home(), "batches", r.BatchID+".json"), b)
			}
			manifest := filepath.Join(t.TempDir(), "manifest.json")
			writeRetrievalFixtureJSON(t, manifest, m)
			writeRetrievalFixtureJSON(t, filepath.Join(runlog.Home(), "evals", r.ID+".json"), r)
			err = runRetrieval([]string{"samples", "--manifest", manifest, "--eval", r.ID}, &bytes.Buffer{})
			if err == nil {
				t.Fatal("accepted inconsistent batch records")
			}
			if kind != "missing batch" && kind != "missing rerun" && !strings.Contains(err.Error(), cell) {
				t.Fatalf("error = %v; want cell %s", err, cell)
			}
		})
	}
}

func TestRetrievalSamplesCompareRuns(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	m, _, err := eval.RetrievalFixture()
	if err != nil {
		t.Fatal(err)
	}
	m.Spec.Matrix[1].RetrievalVersion = "version"
	r := retrievalSamplesEval(t, m, "test", m.Spec.Repeats, false)
	replacement := retrievalSamplesEval(t, m, "replacement", m.Spec.Repeats, false).Results[1]
	replacement.ID = r.Results[1].ID + ".u1"
	r.Results[1].Status, r.Results[1].Attempts = batch.StatusCanceled, []batch.Attempt{{DidNotStart: "setup"}}
	r.Results = append(r.Results, replacement)
	r.RerunBatchIDs = []string{"rerun"}
	writeRetrievalEvalBatches(t, m, r)
	writeRetrievalFixtureJSON(t, filepath.Join(runlog.Home(), "evals", r.ID+".json"), r)
	dir := t.TempDir()
	manifest, samples := filepath.Join(dir, "manifest.json"), filepath.Join(dir, "samples.json")
	writeRetrievalFixtureJSON(t, manifest, m)
	var out bytes.Buffer
	if err := runRetrieval([]string{"samples", "--manifest", manifest, "--eval", r.ID}, &out); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(samples, out.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	var cells []eval.RetrievalSample
	if err := json.Unmarshal(out.Bytes(), &cells); err != nil {
		t.Fatal(err)
	}
	if len(cells[1].RunRefs) != 1 || cells[1].RunRefs[0] != replacement.Attempts[0].RunID {
		t.Fatalf("replacement = %+v", cells[1])
	}
	out.Reset()
	if err := runRetrieval([]string{"compare", "--manifest", manifest, "--samples", samples, "--runs", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var comparison eval.RetrievalComparison
	if err := json.Unmarshal(out.Bytes(), &comparison); err != nil {
		t.Fatal(err)
	}
	if comparison.ExpectedPairs != 30 || comparison.ObservedPairs != 30 || comparison.ComparablePairs != 30 {
		t.Fatalf("comparison = %+v", comparison)
	}
}

func TestRetrievalCorpusDigestUTF8(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corpus.json")
	writeRetrievalFixtureJSON(t, path, retrieval.Version{Entries: []retrieval.Entry{{Key: "a\u2028b\x01c\"d\\e<>&é\x7f", Source: "docs/x.md@abc", Content: "line\u2029end"}}})
	var out bytes.Buffer
	if err := runRetrieval([]string{"corpus-digest", path}, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "d7dee35d6b060dba55ab6aa09e9f6d3b20e9b95cd191ff4c8206c52633fdc349\n" {
		t.Fatalf("digest = %q", out.String())
	}
	for _, field := range []string{"key", "source", "content"} {
		t.Run(field, func(t *testing.T) {
			data := []byte(fmt.Sprintf(`{"entries":[{"key":"key","source":"source","content":"content","%s":"`, field))
			data = append(data, 0xff)
			data = append(data, []byte(`"}]}`)...)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := runRetrieval([]string{"corpus-digest", path}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "UTF-8") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
