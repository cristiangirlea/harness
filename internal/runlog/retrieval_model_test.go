package runlog

import (
	"path/filepath"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/retrieval"
)

func TestRetrievalContinuationSeparatesRequestedAndReportedModel(t *testing.T) {
	for _, tc := range []struct {
		name              string
		unfinished, drift bool
	}{
		{name: "provider_resolves_alias"},
		{name: "unfinished_resolved_alias", unfinished: true},
		{name: "reported_model_drift", drift: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			text := testRetrievalText("reference", "fixture", "revision")
			pin := &retrieval.Record{Mode: "required", OriginalPrompt: "request", Scope: retrieval.Scope{TenantID: "tenant", ProjectID: "project", Backend: "api", Model: "requested-alias", Revision: "revision"}, Packet: &retrieval.Packet{VersionID: "v_fixture", Text: text, Bytes: len(text), Digest: retrieval.TextDigest(text), Entries: []retrieval.Entry{{ID: "entry", Source: "fixture", Content: "reference"}}}}
			task := agent.Task{Prompt: "request", Options: agent.Options{Model: "requested-alias", Retrieval: "required", RetrievalTenant: "tenant", RetrievalProject: "project", RetrievalRevision: "revision"}}
			w, err := Create("api", task)
			if err != nil {
				t.Fatal(err)
			}
			conv := agent.Conversation{Model: "resolved-model-v1", Retrieval: pin}
			for name, data := range map[string]any{"retrieval.json": pin, "messages.json": conv} {
				if err := writeJSON(filepath.Join(w.Dir(), name), data); err != nil {
					t.Fatal(err)
				}
			}
			if tc.unfinished {
				if err := w.events.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				reported := conv.Model
				if tc.drift {
					reported = "unexpected-model"
				}
				if err := w.Close(agent.Result{Backend: "api", Model: reported, Retrieval: pin}); err != nil {
					t.Fatal(err)
				}
			}
			_, _, err = LoadConversation(w.ID())
			if (err != nil) != tc.drift {
				t.Fatalf("continuation error=%v; reject=%t", err, tc.drift)
			}
		})
	}
}
