package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/retrieval"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdmissionSourceRuns(t *testing.T) {
	entries := []retrieval.Entry{{Evidence: "run 20261031-123456-abcdef; 20261031-123456-abcdef / 20261030-123456-123abc"}}
	got, err := admissionSourceRuns(entries, []string{"20261029-123456-aaaaaa"})
	if err != nil || strings.Join(got, ",") != "20261029-123456-aaaaaa,20261030-123456-123abc,20261031-123456-abcdef" {
		t.Fatalf("%v %v", got, err)
	}
	if _, err = admissionSourceRuns(nil, []string{"bad"}); err == nil {
		t.Fatal("invalid run accepted")
	}
	if _, err = admissionSourceRuns(nil, nil); err == nil || !strings.Contains(err.Error(), "candidate cites no source runs") {
		t.Fatalf("%v", err)
	}
}
func TestKnowledgeAdmitRefusesAPI(t *testing.T) {
	if err := runKnowledge([]string{"admit", "--version", "v", "--spec", "x", "-b", "api"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "claude or codex") {
		t.Fatal(err)
	}
}

func TestKnowledgeAdmitRecordsFixtureVerdicts(t *testing.T) {
	for _, tc := range []struct{ name, baseline, candidate, want string }{{"admitted", "fail", "pass", "admitted"}, {"rejected", "pass", "fail", "rejected"}, {"pending", "fail", "fail", "pending"}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HRN_HOME", t.TempDir())
			for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.email", "test@example.com"}, {"config", "user.name", "Test"}, {"commit", "--allow-empty", "-qm", "initial"}} {
				cmd := exec.Command("git", args...)
				cmd.Dir = dir
				if b, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git: %v %s", err, b)
				}
			}
			orig, _ := agent.Get("claude")
			agent.Register(scripted{"claude", func(task agent.Task) agent.Result {
				answer := tc.baseline
				if strings.Contains(task.Prompt, "candidate lesson") {
					answer = tc.candidate
				}
				return agent.Result{Status: agent.StatusOK, Final: answer, Turns: 2, ToolCalls: 1, InputTokens: 100, Usage: &agent.UsageMetadata{InputSemantics: "exclusive_cache", Status: "reported"}}
			}})
			defer agent.Register(orig)
			var recorded retrieval.Admission
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/knowledge/active":
					json.NewEncoder(w).Encode(retrieval.Active{VersionID: "baseline"})
				case "/v1/knowledge/inspect":
					json.NewEncoder(w).Encode(retrieval.Version{Entries: []retrieval.Entry{{Evidence: "20261031-123456-abcdef"}}})
				case "/v1/knowledge/admission":
					if err := json.NewDecoder(r.Body).Decode(&recorded); err != nil {
						t.Error(err)
					}
					w.Write([]byte(`{"status":"recorded"}`))
				case "/v1/knowledge/search":
					var q retrieval.Search
					json.NewDecoder(r.Body).Decode(&q)
					content := "baseline lesson"
					if q.VersionID == "candidate" {
						content = "candidate lesson"
					}
					text := fmt.Sprintf("[Reference fixture; source=\"\"; revision=%s; evidence=\"\"]\n%s\n[/Reference]\n", q.Revision, content)
					json.NewEncoder(w).Encode(retrieval.Packet{VersionID: q.VersionID, Entries: []retrieval.Entry{{ID: "fixture", Content: content}}, Text: text, Bytes: len(text), Digest: retrieval.TextDigest(text), QueryDigest: retrieval.TextDigest(q.Query)})
				default:
					t.Errorf("unexpected route %s", r.URL.Path)
				}
			}))
			defer srv.Close()
			t.Setenv("HRN_SERVER_URL", srv.URL)
			t.Setenv("HRN_OPERATOR_TOKEN", strings.Repeat("o", 64))
			t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("t", 64))
			t.Setenv("HRN_SERVER_CA_FILE", "")
			spec := fmt.Sprintf(`{"dir":%q,"mode":"readonly","model":"fixture","retrieval":"required","retrieval_tenant":"t_fixture","retrieval_project":"p_fixture","retrieval_revision":%q,"repeats":2,"matrix":[{"backend":"claude"}],"tasks":[{"id":"one","prompt":"test","expect":"^pass$"}]}`, dir, strings.Repeat("a", 40))
			path := filepath.Join(t.TempDir(), "spec.json")
			if err := os.WriteFile(path, []byte(spec), 0600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := runKnowledge([]string{"admit", "--version", "candidate", "--spec", path, "-b", "claude", "--held-out", "0"}, &out); err != nil {
				t.Fatal(err)
			}
			if recorded.Verdict != tc.want || len(recorded.EvalIDs) != 2 || len(recorded.SourceRuns) != 1 {
				t.Fatalf("admission: %+v", recorded)
			}
			var printed retrieval.Admission
			if err := json.Unmarshal(out.Bytes(), &printed); err != nil || printed.Verdict != tc.want {
				t.Fatalf("output %s: %v", out.String(), err)
			}
		})
	}
}
