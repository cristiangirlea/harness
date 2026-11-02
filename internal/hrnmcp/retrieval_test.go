package hrnmcp

import (
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/retrieval"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestMCPRunUsesTheRequestedRetrievalModeAndRejectsInvalidTransport(t *testing.T) {
	s := connect(t, t.TempDir())
	t.Setenv("HRN_SERVER_URL", "invalid")
	got := s.call(t, "run", `{"prompt":"unchanged","backend":"stub","retrieval":"off"}`)
	if got.IsError || !strings.Contains(got.Text(), "unchanged") {
		t.Fatal(got)
	}
	got = s.call(t, "run", `{"prompt":"fail before backend","backend":"stub","model":"stub-1","retrieval":"required","retrieval_tenant":"t_fixture","retrieval_project":"p_fixture","retrieval_revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	if !got.IsError || !strings.Contains(got.Text(), "retrieval transport configuration invalid") {
		t.Fatal(got)
	}
}

func TestMCPContinueWithRetrievalOnAnotherBackendThanThePinIsRefused(t *testing.T) {
	dir := t.TempDir()
	s := connect(t, dir)
	w, err := runlog.Create("api", agent.Task{Prompt: "p", Dir: dir, Options: agent.Options{Model: "m", Retrieval: "required"}})
	if err != nil {
		t.Fatal(err)
	}
	pin := &retrieval.Record{Mode: "required", Scope: retrieval.Scope{Backend: "api", Model: "m"}, Packet: &retrieval.Packet{VersionID: "kv-7"}}
	if err := w.Close(agent.Result{Backend: "api", Status: agent.StatusOK, Final: "a", Retrieval: pin}); err != nil {
		t.Fatal(err)
	}
	got := s.call(t, "run", `{"prompt":"x","backend":"stub","model":"m","retrieval":"optional","retrieval_tenant":"t","retrieval_project":"p","retrieval_revision":"r","continue_run_id":"`+w.ID()+`"}`)
	if !got.IsError || !strings.Contains(got.Text(), "kv-7") || !strings.Contains(got.Text(), "continue on api") {
		t.Fatal(got)
	}
	if recs, err := runlog.List(10); err != nil || len(recs) != 1 {
		t.Fatalf("a refused continuation must record nothing: %d runs, %v", len(recs), err)
	}
	got = s.call(t, "run", `{"prompt":"x","backend":"stub","continue_run_id":"`+w.ID()+`"}`)
	if !got.IsError || !strings.Contains(got.Text(), "cannot carry over") {
		t.Fatal(got)
	}
	got = s.call(t, "run", `{"prompt":"x","backend":"stub","retrieval":"off","continue_run_id":"`+w.ID()+`"}`)
	if got.IsError || !strings.Contains(got.Text(), "--- request\n> p") {
		t.Fatal(got)
	}
}
