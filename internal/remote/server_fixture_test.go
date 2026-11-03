package remote

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type serverExchange struct {
	Method   string          `json:"method"`
	Path     string          `json:"path"`
	Request  json.RawMessage `json:"request"`
	Status   int             `json:"status"`
	Response json.RawMessage `json:"response"`
}

func TestRemoteClientReplaysTheServerRecording(t *testing.T) {
	files, err := filepath.Glob("testdata/server/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no server fixtures found")
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) { replayServerFixture(t, file) })
	}
}

func replayServerFixture(t *testing.T, file string) {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	replacements := strings.NewReplacer("<tenant-id>", "t_11111111111111111111111111111111", "<other-tenant-id>", "t_22222222222222222222222222222222", "<project-id>", "p_11111111111111111111111111111111", "<job-id>", "j_11111111111111111111111111111111", "<tenant-token>", strings.Repeat("a", 64), "<other-tenant-token>", strings.Repeat("b", 64), "<time>", "2026-01-01T00:00:00Z")
	var exchanges []serverExchange
	fixtureText := strings.ReplaceAll(strings.ReplaceAll(string(raw), `\u003c`, "<"), `\u003e`, ">")
	if err := json.Unmarshal([]byte(replacements.Replace(fixtureText)), &exchanges); err != nil {
		t.Fatal(err)
	}
	if len(exchanges) != 8 {
		t.Fatalf("expected 8 exchanges, got %d", len(exchanges))
	}
	index := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if index >= len(exchanges) {
			t.Error("unexpected request")
			w.WriteHeader(500)
			return
		}
		want := exchanges[index]
		index++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if r.Method != want.Method || r.URL.RequestURI() != want.Path {
			t.Errorf("request %d: got %s %s, want %s %s", index, r.Method, r.URL.RequestURI(), want.Method, want.Path)
		}
		token := strings.Repeat("a", 64)
		if index == 6 || index == 7 {
			token = strings.Repeat("b", 64)
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("request %d: wrong authorization", index)
		}
		if len(body) == 0 {
			body = []byte("null")
		}
		var gotBody, wantBody any
		if json.Unmarshal(body, &gotBody) != nil || json.Unmarshal(want.Request, &wantBody) != nil || !reflect.DeepEqual(gotBody, wantBody) {
			t.Errorf("request %d: body got %s, want %s", index, body, want.Request)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(want.Status)
		_, _ = w.Write(want.Response)
	}))
	defer server.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HRN_SERVER_URL", server.URL)
	t.Setenv("HRN_SERVER_CA_FILE", ca)
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("a", 64))
	client, err := NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	var submission map[string]any
	if err := json.Unmarshal(exchanges[0].Request, &submission); err != nil {
		t.Fatal(err)
	}
	project := "p_11111111111111111111111111111111"
	tenant := "t_11111111111111111111111111111111"
	job := "j_11111111111111111111111111111111"
	scope := "?tenant_id=" + tenant + "&project_id=" + project
	post := "/v1/projects/" + project + "/jobs"
	paths := []string{post, post, post, "/v1/jobs/" + job + scope, "/v1/jobs/" + job + "/events" + scope + "&after=0", "/v1/jobs/" + job + scope, "/v1/jobs/" + job + "?tenant_id=t_22222222222222222222222222222222&project_id=" + project, "/v1/jobs/" + job + "/cancel" + scope}
	methods := []string{http.MethodPost, http.MethodPost, http.MethodPost, http.MethodGet, http.MethodGet, http.MethodGet, http.MethodGet, http.MethodPost}
	for i, exchange := range exchanges {
		if exchange.Method != methods[i] || exchange.Path != paths[i] {
			t.Fatalf("exchange %d: recorded %s %s, want %s %s", i, exchange.Method, exchange.Path, methods[i], paths[i])
		}
		if i == 5 {
			t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("b", 64))
			client, err = NewFromEnv()
			if err != nil {
				t.Fatal(err)
			}
		}
		if i == 7 {
			t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("a", 64))
			client, err = NewFromEnv()
			if err != nil {
				t.Fatal(err)
			}
		}
		var input any
		if i < 3 {
			if i == 2 {
				submission["prompt"] = "different"
			}
			input = submission
		}
		got, err := client.Request(context.Background(), methods[i], paths[i], input)
		if exchange.Status < 200 || exchange.Status >= 300 {
			var status *HTTPError
			if !errors.As(err, &status) || status.Status != exchange.Status {
				t.Fatalf("exchange %d: expected HTTP %d, got %v", i, exchange.Status, err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		var actual, expected any
		if err := json.Unmarshal(got, &actual); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(exchange.Response, &expected); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(actual, expected) {
			t.Fatalf("exchange %d: response differs", i)
		}
	}
	if index != len(exchanges) {
		t.Fatalf("served %d of %d requests", index, len(exchanges))
	}
}
