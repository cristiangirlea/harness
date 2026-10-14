package hrnmcp

import (
	"strings"
	"testing"
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
