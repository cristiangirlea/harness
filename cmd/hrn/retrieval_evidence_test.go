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
	"github.com/cristiangirlea/harness/internal/retrieval"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// All records below are synthetic disk fixtures, never production evidence.
func TestRetrievalMeasuredRunValidation(t *testing.T) {
	for _, name := range []string{"valid", "off_candidate", "duplicate_run", "dummy_ref", "wrong_effort", "wrong_revision", "wrong_version", "missing_packet", "lost_failed_attempt", "valid_failed_attempt", "missing_status", "wrong_original"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			m, cells, err := eval.RetrievalFixture()
			if err != nil {
				t.Fatal(err)
			}
			m.EvidenceKind = "measured"
			m.Spec.Matrix[1].RetrievalVersion = "v_fixture"
			cells = cells[:2]
			put := func(path string, v any) {
				t.Helper()
				b, e := json.Marshal(v)
				if e != nil {
					t.Fatal(e)
				}
				if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(path, b, 0600); e != nil {
					t.Fatal(e)
				}
			}
			for i := range cells {
				v := m.Spec.Matrix[i]
				task := agent.Task{Prompt: m.Spec.Tasks[0].Prompt, Options: agent.Options{Model: v.Model, Effort: v.Effort, Retrieval: v.Retrieval, RetrievalRevision: m.Revision, RetrievalTenant: "tenant", RetrievalProject: "project", RetrievalVersion: v.RetrievalVersion}}
				res := cells[i].Usage.Attempts[0]
				if name == "missing_status" {
					res.Status = ""
				}
				if i == 1 && name != "off_candidate" {
					text := fmt.Sprintf("[Reference %s; source=%q; revision=%s; evidence=%q]\n%s\n[/Reference]\n", "entry", "", m.Revision, "", "ref")
					res.Retrieval = &retrieval.Record{Mode: v.Retrieval, OriginalPrompt: task.Prompt, Scope: retrieval.Scope{TenantID: "tenant", ProjectID: "project", Backend: v.Backend, Model: v.Model, Revision: m.Revision}, Packet: &retrieval.Packet{VersionID: v.RetrievalVersion, Text: text, Bytes: len(text), Digest: retrieval.TextDigest(text), Entries: []retrieval.Entry{{ID: "entry", Content: "ref"}}}}
					if name == "wrong_version" {
						res.Retrieval.Packet.VersionID = "v_other"
					}
					if name == "missing_packet" {
						res.Retrieval.Packet = nil
					}
					if name == "wrong_original" {
						res.Retrieval.OriginalPrompt = "other"
					}
				}
				if i == 1 && name == "off_candidate" {
					task.Retrieval = "off"
				}
				if i == 1 && name == "wrong_effort" {
					task.Effort = "other"
				}
				if i == 0 && name == "wrong_revision" {
					task.RetrievalRevision = "other"
				}
				w, e := runlog.Create(v.Backend, task)
				if e != nil {
					t.Fatal(e)
				}
				if res.Retrieval != nil {
					put(filepath.Join(w.Dir(), "retrieval.json"), res.Retrieval)
				}
				if e = w.Close(res); e != nil {
					t.Fatal(e)
				}
				cells[i].RunRefs = []string{w.ID()}
				if i == 0 && (name == "lost_failed_attempt" || name == "valid_failed_attempt") {
					failed := res
					failed.Status = agent.StatusError
					fw, e := runlog.Create(v.Backend, task)
					if e != nil {
						t.Fatal(e)
					}
					if e = fw.Close(failed); e != nil {
						t.Fatal(e)
					}
					br := batch.Result{ID: "fixture-batch", Base: m.Revision, Tasks: []batch.TaskResult{{ID: "fixture-task", Status: batch.StatusPass, Attempts: []batch.Attempt{{RunID: fw.ID(), Result: failed}, {RunID: w.ID(), Result: res}}}}}
					put(filepath.Join(runlog.Home(), "batches", "fixture-batch.json"), br)
					if name == "valid_failed_attempt" {
						cells[i].RunRefs = []string{fw.ID(), w.ID()}
					}
				}
			}
			if name == "duplicate_run" {
				cells = append(cells, cells[0])
			}
			if name == "dummy_ref" {
				cells[0].RunRefs = []string{"dummy"}
			}
			err = resolveRetrievalRuns(m, cells)
			valid := name == "valid" || name == "valid_failed_attempt"
			if (err == nil) != valid {
				t.Fatalf("validation err=%v", err)
			}
			if name == "valid_failed_attempt" && len(cells[0].Usage.Attempts) != 2 {
				t.Fatal("failed attempt lost")
			}
		})
	}
}

func TestRetrievalMeasuredCommandAutomaticallyResolves(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	m, s, err := eval.RetrievalFixture()
	if err != nil {
		t.Fatal(err)
	}
	m.EvidenceKind = "measured"
	m.Spec.Matrix[1].RetrievalVersion = "v_fixture"
	s[0].RunRefs = []string{"dummy"}
	dir := t.TempDir()
	for name, v := range map[string]any{"manifest.json": m, "samples.json": s} {
		b, _ := json.Marshal(v)
		if err := os.WriteFile(filepath.Join(dir, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	err = runRetrieval([]string{"compare", "--manifest", filepath.Join(dir, "manifest.json"), "--samples", filepath.Join(dir, "samples.json"), "--json"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "measured run dummy") {
		t.Fatalf("dummy refs accepted without --runs: %v", err)
	}
}
