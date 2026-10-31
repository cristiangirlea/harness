package followup

import (
	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/retrieval"
	"strings"
	"testing"
)

func TestFreshPinFillsRetrievalBeforeRecording(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	id := finished(t, "", agent.Task{Dir: workspace(t)}, agent.Result{Backend: "codex", Retrieval: &retrieval.Record{Mode: "required", Scope: retrieval.Scope{TenantID: "tenant", ProjectID: "project", Backend: "codex", Model: "model", Revision: "rev"}, Packet: &retrieval.Packet{VersionID: "v1"}}})
	p, err := Make(Request{RunID: id})
	if err != nil {
		t.Fatal(err)
	}
	opts := agent.Options{}
	p.ApplyRetrieval(&opts)
	if opts.Retrieval != "required" || opts.RetrievalTenant != "tenant" || opts.RetrievalProject != "project" || opts.RetrievalRevision != "rev" || opts.Model != "model" || opts.RetrievalVersion != "v1" {
		t.Fatalf("options: %+v", opts)
	}
}

func TestRefreshRequiresFreshContinuationAndPin(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := workspace(t)
	empty := finished(t, "", agent.Task{Dir: dir}, agent.Result{Backend: "codex", Session: "s"})
	if _, err := Make(Request{RunID: empty, RetrievalRefresh: true}); err == nil {
		t.Error("refresh without a pin accepted")
	}
	pinned := finished(t, "", agent.Task{Dir: dir}, agent.Result{Backend: "codex", Session: "s", Retrieval: &retrieval.Record{Mode: "required", Scope: retrieval.Scope{Backend: "codex", Revision: "old"}, Packet: &retrieval.Packet{VersionID: "v1"}}})
	if _, err := Make(Request{RunID: pinned, Resume: true, RetrievalRefresh: true}); err == nil {
		t.Error("refresh on resumed session accepted")
	}
	p, err := Make(Request{RunID: pinned, RetrievalRefresh: true})
	if err != nil || p.RetrievalVersion != "" || !p.retrievalRefreshed {
		t.Fatalf("refresh = %+v, %v", p, err)
	}
}

func TestBackendSwitchRequiresExplicitRetrievalOff(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	id := finished(t, "", agent.Task{Dir: workspace(t)}, agent.Result{Backend: "codex", Retrieval: &retrieval.Record{Packet: &retrieval.Packet{VersionID: "v1"}}})
	for _, fresh := range []bool{false, true} {
		if _, err := Make(Request{RunID: id, Backend: "claude", Fresh: fresh}); err == nil {
			t.Errorf("switch fresh=%v accepted", fresh)
		}
		if _, err := Make(Request{RunID: id, Backend: "claude", Fresh: fresh, Retrieval: "off"}); err != nil {
			t.Errorf("off fresh=%v: %v", fresh, err)
		}
	}
}

func TestRetrievalDecisionIsRecordedInRouteNote(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	id := finished(t, "", agent.Task{Dir: workspace(t)}, agent.Result{Backend: "codex", Retrieval: &retrieval.Record{Mode: "required", Scope: retrieval.Scope{Backend: "codex", Revision: "old"}, Packet: &retrieval.Packet{VersionID: "v1"}}})
	for _, refresh := range []bool{false, true} {
		p, err := Make(Request{RunID: id, RetrievalRefresh: refresh})
		if err != nil {
			t.Fatal(err)
		}
		note := p.RetrievalNote("existing")
		if !strings.Contains(note, "existing") || !strings.Contains(note, "v1") || (refresh && !strings.Contains(note, "refreshed")) {
			t.Fatalf("note: %q", note)
		}
	}
}

func TestCallerRevisionMustMatchPinUnlessRefreshed(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	id := finished(t, "", agent.Task{Dir: workspace(t)}, agent.Result{Backend: "codex", Retrieval: &retrieval.Record{Mode: "required", Scope: retrieval.Scope{Backend: "codex", Revision: "old"}, Packet: &retrieval.Packet{VersionID: "v1"}}})
	if _, err := Make(Request{RunID: id, RetrievalRevision: "new"}); err == nil || !strings.Contains(err.Error(), "old") || !strings.Contains(err.Error(), "new") {
		t.Fatalf("mismatch: %v", err)
	}
	p, err := Make(Request{RunID: id, RetrievalRevision: "new", RetrievalRefresh: true})
	if err != nil {
		t.Fatal(err)
	}
	opts := agent.Options{}
	p.ApplyRetrieval(&opts)
	if opts.RetrievalRevision != "new" || opts.RetrievalVersion != "" {
		t.Fatalf("options: %+v", opts)
	}
}

func TestFreshPinRejectsChangesToItsOtherScope(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	id := finished(t, "", agent.Task{Dir: workspace(t)}, agent.Result{Backend: "codex", Retrieval: &retrieval.Record{Mode: "required", Scope: retrieval.Scope{TenantID: "tenant", ProjectID: "project", Backend: "codex", Model: "model", Revision: "rev"}, Packet: &retrieval.Packet{VersionID: "v1"}}})
	for _, req := range []Request{{Model: "other"}, {RetrievalTenant: "other"}, {RetrievalProject: "other"}, {RetrievalVersion: "other"}} {
		req.RunID = id
		if _, err := Make(req); err == nil {
			t.Errorf("accepted scope change: %+v", req)
		}
	}
}

func TestFreshPinWithoutRetrievalModeIsRefused(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	id := finished(t, "", agent.Task{Dir: workspace(t)}, agent.Result{Backend: "codex", Retrieval: &retrieval.Record{Packet: &retrieval.Packet{VersionID: "v1"}}})
	if _, err := Make(Request{RunID: id}); err == nil {
		t.Fatal("pin without mode was silently dropped")
	}
}

func TestFreshPinRejectsConflictingBackendScope(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	id := finished(t, "", agent.Task{Dir: workspace(t)}, agent.Result{Backend: "codex", Retrieval: &retrieval.Record{Mode: "required", Scope: retrieval.Scope{Backend: "claude"}, Packet: &retrieval.Packet{VersionID: "v1"}}})
	if _, err := Make(Request{RunID: id}); err == nil {
		t.Fatal("conflicting pin backend accepted")
	}
}
