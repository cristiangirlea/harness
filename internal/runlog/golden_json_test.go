package runlog

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/retrieval"
)

// Set UPDATE_GOLDEN=1 when deliberately updating the JSON contract fixtures.
func TestGoldenRecordJSON(t *testing.T) {
	now := time.Date(2024, 2, 3, 4, 5, 6, 7000000, time.UTC)
	rec := Record{StartHead: "start", RoutedBy: &RoutedBy{Category: "category", Declared: true, Eval: "eval", Set: "set"}, RouteNote: "route", ID: "id", Owner: "owner", Backend: "backend", Started: now, Task: agent.Task{Prompt: "prompt", Dir: "dir", Options: agent.Options{Model: "model", Timeout: agent.Duration(42 * time.Second)}}, Result: agent.Result{Backend: "backend", Status: agent.StatusOK, Retrieval: &retrieval.Record{Mode: "required", OriginalPrompt: "prompt", Scope: retrieval.Scope{TenantID: "tenant", ProjectID: "project", Backend: "backend", Model: "model", Revision: "revision"}}}, Continues: "previous", ContinueRoute: "continue-route", RecordError: "record-error", Done: true, Setup: &agent.Setup{Backend: "backend", Digest: "digest", BaseDigest: "base", Files: map[string]string{"file": "hash"}, MCP: []string{"mcp"}, Plugins: []string{"plugin"}, Skills: []string{"skill"}, Truncated: true}}
	for name, value := range map[string]Record{"record": rec, "record_zero": {}} {
		t.Run(name, func(t *testing.T) {
			got, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("testdata", "golden", name+".json")
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("JSON differs from %s\ngot: %s\nwant: %s", path, got, want)
			}
		})
	}
}

func TestStoredRecordFixtures(t *testing.T) {
	cases := []struct {
		name string
		want Record
	}{
		{"legacy_timeout", Record{ID: "legacy", Backend: "api", Task: agent.Task{Prompt: "old prompt", Dir: "old-dir", Options: agent.Options{Mode: agent.ModeAsk, Timeout: agent.Duration(90 * time.Second)}}}},
		{"without_retrieval", Record{ID: "without", Backend: "api", Task: agent.Task{Prompt: "plain", Dir: "dir"}, Result: agent.Result{Backend: "api", Status: agent.StatusOK}}},
		{"with_retrieval", Record{ID: "with", Backend: "api", Task: agent.Task{Prompt: "query", Dir: "dir", Options: agent.Options{Retrieval: "required"}}, Result: agent.Result{Backend: "api", Status: agent.StatusOK, Retrieval: &retrieval.Record{Mode: "required", OriginalPrompt: "query", Scope: retrieval.Scope{TenantID: "tenant", ProjectID: "project", Backend: "api", Model: "model", Revision: "revision"}, Packet: &retrieval.Packet{VersionID: "version", AuditID: "audit", Entries: []retrieval.Entry{{Key: "key", Content: "content", Source: "source", Evidence: "evidence", Identifiers: []string{"id"}}}, Text: "text", Bytes: 4, Digest: "digest", QueryDigest: "query", Ranking: "rank", DurationNS: 123}, Replayed: true, DurationNS: 456}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", "golden", tc.name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var got Record
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("decoded record:\n got: %#v\nwant: %#v", got, tc.want)
			}
		})
	}
}
