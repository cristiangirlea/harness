package runlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/retrieval"
)

func testRetrievalText(content, source, revision string) string {
	return fmt.Sprintf("[Reference %s; source=%q; revision=%s; evidence=%q]\n%s\n[/Reference]\n", "entry", source, revision, "", content)
}

func TestRetrievalConversationRecoveryAndConsistency(t *testing.T) {
	for _, tc := range []struct {
		name        string
		unfinished  bool
		unavailable bool
		mutate      func(*Record, *agent.Conversation, **retrieval.Record)
		reject      bool
	}{
		{name: "complete"}, {name: "unfinished", unfinished: true},
		{name: "optional_unavailable", unavailable: true},
		{name: "optional_unavailable_unfinished", unavailable: true, unfinished: true},
		{name: "missing_result_pin", mutate: func(r *Record, c *agent.Conversation, p **retrieval.Record) { r.Result.Retrieval = nil }, reject: true},
		{name: "missing_conversation_pin", mutate: func(r *Record, c *agent.Conversation, p **retrieval.Record) { c.Retrieval = nil }, reject: true},
		{name: "missing_persisted_pin", unfinished: true, mutate: func(r *Record, c *agent.Conversation, p **retrieval.Record) { *p = nil }, reject: true},
		{name: "corrupt_digest", unfinished: true, mutate: func(r *Record, c *agent.Conversation, p **retrieval.Record) { (*p).Packet.Digest = "corrupt" }, reject: true},
		{name: "replaced_packet", mutate: func(r *Record, c *agent.Conversation, p **retrieval.Record) {
			c.Retrieval.Packet.Text = "replaced"
			c.Retrieval.Packet.Bytes = 8
			c.Retrieval.Packet.Digest = retrieval.TextDigest("replaced")
		}, reject: true},
		{name: "replaced_source", mutate: func(r *Record, c *agent.Conversation, p **retrieval.Record) {
			c.Retrieval.Packet.Entries[0].Source = "other"
		}, reject: true},
		{name: "replaced_scope", mutate: func(r *Record, c *agent.Conversation, p **retrieval.Record) { c.Retrieval.Scope.ProjectID = "other" }, reject: true},
		{name: "replaced_mode", mutate: func(r *Record, c *agent.Conversation, p **retrieval.Record) { c.Retrieval.Mode = "optional" }, reject: true},
		{name: "optional_unavailable_replaced_scope", unavailable: true, mutate: func(r *Record, c *agent.Conversation, p **retrieval.Record) { c.Retrieval.Scope.ProjectID = "other" }, reject: true},
		{name: "optional_unavailable_replaced_mode", unavailable: true, mutate: func(r *Record, c *agent.Conversation, p **retrieval.Record) { c.Retrieval.Mode = "required" }, reject: true},
		{name: "optional_unavailable_replaced_failure", unavailable: true, mutate: func(r *Record, c *agent.Conversation, p **retrieval.Record) { c.Retrieval.Failure = "other" }, reject: true},
		{name: "optional_unavailable_one_sided_packet", unavailable: true, mutate: func(r *Record, c *agent.Conversation, p **retrieval.Record) {
			// Passes the single-pin checks, so only the packet comparison can reject it.
			text := testRetrievalText("ref", "fixture", "revision")
			c.Retrieval.Packet = &retrieval.Packet{VersionID: "v_fixture", Text: text, Bytes: len(text), Digest: retrieval.TextDigest(text), Entries: []retrieval.Entry{{ID: "entry", Source: "fixture", Content: "ref"}}}
		}, reject: true},
		{name: "optional_unavailable_missing_conversation_pin", unavailable: true, mutate: func(r *Record, c *agent.Conversation, p **retrieval.Record) { c.Retrieval = nil }, reject: true},
		{name: "optional_unavailable_persisted_scope", unavailable: true, unfinished: true, mutate: func(r *Record, c *agent.Conversation, p **retrieval.Record) { (*p).Scope.Revision = "other" }, reject: true},
		{name: "optional_unavailable_missing_persisted_pin", unavailable: true, unfinished: true, mutate: func(r *Record, c *agent.Conversation, p **retrieval.Record) { *p = nil }, reject: true},
		{name: "task_scope", mutate: func(r *Record, c *agent.Conversation, p **retrieval.Record) { r.Task.RetrievalProject = "other" }, reject: true},
		{name: "both_pins_corrupt", mutate: func(r *Record, c *agent.Conversation, p **retrieval.Record) {
			r.Result.Retrieval.Packet.Digest = "bad"
			c.Retrieval.Packet.Digest = "bad"
			(*p).Packet.Digest = "bad"
		}, reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			text := testRetrievalText("ref", "fixture", "revision")
			pin := retrieval.Record{Mode: "required", OriginalPrompt: "task", Scope: retrieval.Scope{TenantID: "tenant", ProjectID: "project", Backend: "api", Model: "fixture", Revision: "revision"}, Packet: &retrieval.Packet{VersionID: "v_fixture", Text: text, Bytes: len(text), Digest: retrieval.TextDigest(text), Entries: []retrieval.Entry{{ID: "entry", Source: "fixture", Content: "ref"}}}}
			if tc.unavailable {
				pin.Mode, pin.Failure, pin.Packet = "optional", "optional retrieval unavailable", nil
			}
			clone := func() *retrieval.Record {
				b, _ := json.Marshal(pin)
				var p retrieval.Record
				json.Unmarshal(b, &p)
				return &p
			}
			rec := Record{ID: "fixture", Backend: "api", Task: agent.Task{Prompt: "task", Options: agent.Options{Model: "fixture", Retrieval: "required", RetrievalTenant: "tenant", RetrievalProject: "project", RetrievalRevision: "revision", RetrievalVersion: "v_fixture"}}, Result: agent.Result{Retrieval: clone()}}
			if tc.unavailable {
				rec.Task.Retrieval, rec.Task.RetrievalVersion = "optional", ""
			}
			conv := agent.Conversation{Model: "fixture", Retrieval: clone()}
			persisted := clone()
			if tc.mutate != nil {
				tc.mutate(&rec, &conv, &persisted)
			}
			dir := filepath.Join(Root(), rec.ID)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			put := func(name string, v any) {
				t.Helper()
				if err := writeJSON(filepath.Join(dir, name), v); err != nil {
					t.Fatal(err)
				}
			}
			taskRecord := rec
			taskRecord.Result = agent.Result{}
			put("task.json", taskRecord)
			put("messages.json", conv)
			if !tc.unfinished {
				put("result.json", rec)
			}
			if persisted != nil {
				put("retrieval.json", persisted)
			}
			got, history, err := LoadConversation(rec.ID)
			if (err != nil) != tc.reject {
				t.Fatalf("err=%v reject=%t", err, tc.reject)
			}
			if err == nil && (got.Done == tc.unfinished || got.Result.Retrieval == nil || history.Retrieval == nil) {
				t.Fatal("lost pin or completion state")
			}
			if err == nil && tc.unavailable && (!reflect.DeepEqual(got.Result.Retrieval, &pin) || !reflect.DeepEqual(history.Retrieval, &pin)) {
				t.Fatal("optional retrieval failure, scope or absent packet changed on load")
			}
		})
	}
}

func TestRetrievalContinuationSymmetricPresence(t *testing.T) {
	if agent.ValidateContinuationPin(nil, &agent.Conversation{}) != nil {
		t.Fatal("legacy conversation rejected")
	}
	if agent.ValidateContinuationPin(nil, &agent.Conversation{Retrieval: &retrieval.Record{}}) == nil {
		t.Fatal("one-sided pin accepted")
	}
	if agent.ValidateContinuationPin(&retrieval.Record{}, &agent.Conversation{}) == nil {
		t.Fatal("one-sided pin accepted")
	}
}
