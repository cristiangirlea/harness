package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestPublicProjectGrantsAndAudit(t *testing.T) {
	f := newAccessFixture(t)
	t.Setenv("HRN_OWNER", "local:me")
	project := t.TempDir()
	w, err := runlog.Create("fake", agent.Task{Dir: filepath.Join(project, ".hrn", "worktrees", "task"), Prompt: "shared project"})
	if err != nil {
		t.Fatal(err)
	}
	w.Event(agent.Event{Kind: agent.EventToolCall, Tool: "shell", Text: "shared tool"})
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	batchID := "20261020-120000-aabbcc"
	saveBatch(t, batch.Result{ID: batchID, Dir: project, Started: time.Now(), Duration: time.Second, Tasks: []batch.TaskResult{{ID: "task", Status: batch.StatusPass, Attempts: []batch.Attempt{{RunID: w.ID()}}}}})
	data, _ := json.Marshal([]map[string]string{{"viewer": "email:reader@example.com", "project": project}})
	if err := os.WriteFile(filepath.Join(runlog.Home(), "grants.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	f.opt.OwnerEmails = []string{"owner@example.com"}
	h := New(context.Background(), f.opt)
	for _, id := range []string{w.ID(), batchID} {
		kind := "run"
		if id == batchID {
			kind = "batch"
		}
		for _, path := range []string{"/" + kind + "/" + id, "/flow/" + id, "/flow/" + id + "/json", "/stream/" + kind + "/" + id, "/stream/flow/" + id} {
			for _, tc := range []struct {
				email, decision, basis string
				status                 int
			}{
				{"reader@example.com", "allowed", "grant:" + filepath.Clean(project), 200},
				{"other@example.com", "denied", "none", 404},
				{"owner@example.com", "allowed", "owner", 200},
			} {
				resp := f.request(h, path, "harness.example.com", "", f.tokenFor(tc.email))
				if resp.Code != tc.status {
					t.Fatalf("%s %s: %d %s", tc.email, path, resp.Code, resp.Body)
				}
				row := lastAccessRow(t)
				if row["record"] != id || row["decision"] != tc.decision || row["basis"] != tc.basis {
					t.Fatalf("%s %s: row = %v", tc.email, path, row)
				}
				if strings.HasPrefix(path, "/stream/") && tc.status == 200 {
					data, err := os.ReadFile(filepath.Join(runlog.Home(), "watch-access.log"))
					if err != nil {
						t.Fatal(err)
					}
					lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
					var start map[string]any
					if err := json.Unmarshal(lines[len(lines)-2], &start); err != nil {
						t.Fatal(err)
					}
					if start["stream"] != "start" || row["stream"] != "end" || start["record"] != id || start["decision"] != tc.decision || start["basis"] != tc.basis {
						t.Fatalf("stream audit: start=%v, end=%v", start, row)
					}
				}
			}
		}
	}
	resp := f.request(h, "/", "harness.example.com", "", f.tokenFor("reader@example.com"))
	if !strings.Contains(resp.Body.String(), w.ID()) || !strings.Contains(resp.Body.String(), batchID) {
		t.Fatal("granted index omitted records")
	}
	missingID := "20260101-000000-aaaaaa"
	resp = f.request(h, "/run/"+missingID, "harness.example.com", "", f.tokenFor("reader@example.com"))
	row := lastAccessRow(t)
	if resp.Code != 404 || row["record"] != missingID || row["decision"] != "denied" || row["basis"] != "none" {
		t.Fatalf("missing: %d %v", resp.Code, row)
	}
}

func TestPublicMalformedGrantsDenyAndReportOnce(t *testing.T) {
	f := newAccessFixture(t)
	t.Setenv("HRN_OWNER", "local:me")
	id := saveRun(t, "fake", "private", &agent.Result{Status: agent.StatusOK})
	if err := os.WriteFile(filepath.Join(runlog.Home(), "grants.json"), []byte(`[{`), 0600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	f.opt.Public.Errors = &stderr
	f.opt.OwnerEmails = []string{"owner@example.com"}
	h := New(context.Background(), f.opt)
	for range 2 {
		resp := f.request(h, "/run/"+id, "harness.example.com", "", f.tokenFor("reader@example.com"))
		row := lastAccessRow(t)
		if resp.Code != 404 || row["record"] != id || row["decision"] != "denied" || row["basis"] != "none" {
			t.Fatalf("denied: %d %v", resp.Code, row)
		}
	}
	if strings.Count(stderr.String(), "grants.json") != 1 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
