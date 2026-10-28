package watch

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIndexShowsLastAutomaticGCPass(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "gc.log"), []byte("{\"time\":\"2026-10-28T12:00:00Z\",\"scheduled\":true,\"actions\":[{\"path\":\"private\",\"bytes\":42,\"reason\":\"idle\",\"kind\":\"remove\"}]}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	w := backendRequest(New(context.Background(), Options{}), http.MethodGet, "/", "127.0.0.1:1234", "127.0.0.1:8089", "", "", "")
	if !strings.Contains(w.Body.String(), "Last automatic GC") || !strings.Contains(w.Body.String(), "42") {
		t.Fatalf("missing pass: %s", w.Body.String())
	}
}
