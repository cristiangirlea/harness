package watch

import (
	"context"
	"runtime"
	"sync"

	"encoding/json"
	"github.com/cristiangirlea/harness/internal/prefs"
	"github.com/cristiangirlea/harness/internal/runlog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackendActionAudit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	s := &server{}
	post := func(body string) int {
		r := httptest.NewRequest(http.MethodPost, "http://localhost/backends", strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:1234"
		w := httptest.NewRecorder()
		s.saveBackends(w, r)
		return w.Code
	}
	if got := post(`{"enabled":["claude"],"default":"claude"}`); got != http.StatusNoContent {
		t.Fatalf("save: %d", got)
	}
	if got := post(`{"enabled":["not-a-backend"]}`); got != http.StatusBadRequest {
		t.Fatalf("refusal: %d", got)
	}
	b, err := os.ReadFile(filepath.Join(home, "watch-audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 {
		t.Fatalf("audit lines: %d", len(lines))
	}
	for i, line := range lines {
		var row struct {
			Client string `json:"client"`
			Action string `json:"action"`
			Result string `json:"result"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if row.Client != "loopback" || row.Action != "backends" || row.Result == "" || (i == 0 && row.Result != "intent") || (i == 1 && row.Result != "saved") || (i == 2 && row.Result != "refused") {
			t.Fatalf("audit row %d: %+v", i, row)
		}
	}
}

func TestBackendActionAuditFailureDoesNotSave(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	original := []byte(`{"enabled":["codex"]}`)
	if err := os.WriteFile(filepath.Join(home, "backends.json"), original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "watch-audit.jsonl"), 0700); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "http://localhost/backends", strings.NewReader(`{"enabled":["claude"]}`))
	r.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	(&server{}).saveBackends(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: %d", w.Code)
	}
	got, err := os.ReadFile(filepath.Join(home, "backends.json"))
	if err != nil || string(got) != string(original) {
		t.Fatalf("preferences changed: %q, %v", got, err)
	}
}

func TestActionGuardAndRateLimit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	h := New(context.Background(), Options{Key: "key"})
	bad := backendRequest(h, "POST", "/backends", "127.0.0.1:1", "evil.example", "key", "", `{}`)
	if bad.Code != http.StatusForbidden {
		t.Fatalf("guard: %d", bad.Code)
	}
	for i := 0; i < 11; i++ {
		w := backendRequest(h, "POST", "/backends", "127.0.0.1:1", "localhost", "key", "", `{}`)
		want := http.StatusNoContent
		if i == 10 {
			want = http.StatusTooManyRequests
		}
		if w.Code != want {
			t.Fatalf("request %d: %d", i, w.Code)
		}
	}
	data, err := os.ReadFile(filepath.Join(home, "watch-audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"client":"unverified"`) || !strings.Contains(string(data), `"reason":"rate limit"`) || strings.Contains(string(data), "127.0.0.1") {
		t.Fatalf("audit: %s", data)
	}
}

func TestRecentActionVisibilityAndInterrupted(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	row, err := newIntent("<client>", prefs.Prefs{}, prefs.Prefs{EnabledNames: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := appendAction(row); err != nil {
		t.Fatal(err)
	}
	h := New(context.Background(), Options{LAN: []Endpoint{{Addr: "192.168.1.2:8089"}}})
	local := backendRequest(h, "GET", "/", "127.0.0.1:1", "localhost", "", "", "").Body.String()
	if !strings.Contains(local, "interrupted") || !strings.Contains(local, "&lt;client&gt;") || strings.Contains(local, "<client>") {
		t.Fatalf("local audit: %s", local)
	}
	remote := backendRequest(h, "GET", "/", "192.168.1.3:1", "192.168.1.2:8089", "", "", "").Body.String()
	if strings.Contains(remote, "Recent changes") {
		t.Fatal("audit visible remotely")
	}
}

func TestActionSerialization(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	s := &server{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest("POST", "http://localhost/backends", strings.NewReader(`{"enabled":["claude"]}`))
			r.RemoteAddr = "127.0.0.1:1"
			w := httptest.NewRecorder()
			s.saveBackends(w, r)
			if w.Code != http.StatusNoContent {
				t.Errorf("save: %d", w.Code)
			}
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(filepath.Join(runlog.Home(), "watch-audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 16 {
		t.Fatalf("lines: %d", len(lines))
	}
	for i := 0; i < len(lines); i += 2 {
		var a, b actionRow
		if json.Unmarshal([]byte(lines[i]), &a) != nil || json.Unmarshal([]byte(lines[i+1]), &b) != nil || a.Result != "intent" || b.Result != "saved" || a.ID != b.ID {
			t.Fatalf("pair %d: %s %s", i, lines[i], lines[i+1])
		}
	}
}

func TestActionFileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions")
	}
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("HRN_HOME", home)
	if err := appendAction(actionRow{Result: "refused"}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{home: 0700, filepath.Join(home, "watch-audit.jsonl"): 0600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("mode %s: %v %v", path, info, err)
		}
	}
}
