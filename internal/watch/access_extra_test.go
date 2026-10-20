package watch

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestAPublicViewerCannotTellABrokenRecordFromAMissingOne(t *testing.T) {
	f := newAccessFixture(t)
	t.Setenv("HRN_OWNER", "local:test")
	broken, absent := "20261020-140000-aabbcc", "20261020-140000-ffffff"
	if err := os.MkdirAll(filepath.Join(runlog.Root(), broken), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runlog.Root(), broken, "task.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.opt.OwnerEmails = []string{"owner@example.com"}
	h := New(context.Background(), f.opt)
	token := f.tokenFor("other@example.com")
	missing := f.request(h, "/run/"+absent, "harness.example.com", "", token)
	got := f.request(h, "/run/"+broken, "harness.example.com", "", token)
	same := strings.ReplaceAll(got.Body.String(), broken, "ID") == strings.ReplaceAll(missing.Body.String(), absent, "ID")
	if got.Code != missing.Code || !same {
		t.Fatalf("a broken record answered %d (same body %v), a missing one %d", got.Code, same, missing.Code)
	}
}

func TestTheFlowPageWorksWithNoSecretInTheEnvironment(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:test")
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		up := strings.ToUpper(name)
		for _, w := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL"} {
			if name != "" && strings.Contains(up, w) {
				t.Setenv(name, "")
			}
		}
	}
	id := saveRun(t, "fake", "prompt", &agent.Result{Status: agent.StatusOK})
	srv := httptest.NewServer(New(context.Background(), Options{}))
	defer srv.Close()
	for _, path := range []string{"/flow/" + id, "/flow/" + id + "/json"} {
		if code, _ := get(t, srv.URL+path); code != 200 {
			t.Errorf("%s: %d", path, code)
		}
	}
}
