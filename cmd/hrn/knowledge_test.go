package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKnowledgeCommandsUseReadAndOperatorCredentialsByRole(t *testing.T) {
	operator := strings.Repeat("o", 64)
	read := strings.Repeat("r", 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := read
		if r.URL.Path == "/v1/knowledge/change" || r.URL.Path == "/v1/knowledge/import" {
			want = operator
		}
		if r.Header.Get("Authorization") != "Bearer "+want {
			t.Error("wrong credential role")
		}
		_, _ = w.Write([]byte(`{"generation":1}`))
	}))
	defer srv.Close()
	t.Setenv("HRN_SERVER_URL", srv.URL)
	t.Setenv("HRN_TENANT_TOKEN", read)
	t.Setenv("HRN_OPERATOR_TOKEN", operator)
	t.Setenv("HRN_SERVER_CA_FILE", "")
	path := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(path, []byte(`{"tenant_id":"t_fixture","project_id":"p_fixture","backend":"codex","model":"fixture","revision":"abc"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"active", "import", "activate", "disable", "rollback"} {
		if err := runKnowledge([]string{op, path}, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HRN_OPERATOR_TOKEN", "")
	if err := runKnowledge([]string{"activate", path}, &bytes.Buffer{}); err == nil {
		t.Fatal("operator fell back to reader")
	}
	if err := os.WriteFile(path, []byte(`{"bearer_token":"secret"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runKnowledge([]string{"active", path}, &bytes.Buffer{}); err == nil {
		t.Fatal("credential field accepted")
	}
}

func TestKnowledgeSearchRejectsInvalidPacket(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/knowledge/search" {
			t.Errorf("path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"query_digest":"incorrect"}`))
	}))
	defer srv.Close()
	t.Setenv("HRN_SERVER_URL", srv.URL)
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("r", 64))
	t.Setenv("HRN_SERVER_CA_FILE", "")
	path := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(path, []byte(`{"query":"fixture"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runKnowledge([]string{"search", path}, &out); err == nil || !strings.Contains(err.Error(), "query digest") {
		t.Fatalf("invalid search packet accepted or wrong error: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("invalid packet written: %s", out.String())
	}
}
