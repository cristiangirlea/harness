package watch

import (
	"context"
	"errors"
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
	"time"
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

func TestAuditRefusalCap(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	h := New(context.Background(), Options{Key: "key"})
	for i := 0; i < 50; i++ {
		w := backendRequest(h, "POST", "/backends", "192.168.1.3:1", "localhost", "", "", `{}`)
		if i >= 10 && w.Code != http.StatusTooManyRequests {
			t.Fatalf("request %d: %d", i, w.Code)
		}
	}
	data, err := os.ReadFile(filepath.Join(home, "watch-audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 11 {
		t.Fatalf("lines: %d", len(lines))
	}
	if !strings.Contains(lines[10], `"reason":"rate limit"`) {
		t.Fatalf("last: %s", lines[10])
	}
}

func TestAuditClientsAndBuckets(t *testing.T) {
	s := &server{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "http://localhost/backends", nil)
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("X-Forwarded-For", "192.0.2.1")
	if got := s.actionClient(w, r); got != "unverified" {
		t.Fatal(got)
	}
	r.Header.Del("X-Forwarded-For")
	r.RemoteAddr = "192.168.1.3:1"
	if got := s.actionClient(w, r); got != "unverified" {
		t.Fatal(got)
	}
	aw := &accessWriter{email: "Person@Example.COM"}
	if got := s.actionClient(aw, r); got != "person@example.com" {
		t.Fatal(got)
	}
}

func TestRecentActionsExcludeRefusalsAndOldTail(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	row, err := newIntent("loopback", prefs.Prefs{}, prefs.Prefs{})
	if err != nil {
		t.Fatal(err)
	}
	if err := appendAction(row); err != nil {
		t.Fatal(err)
	}
	row.Result = "saved"
	if err := appendAction(row); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := appendAction(actionRow{Result: "refused"}); err != nil {
			t.Fatal(err)
		}
	}
	rows := recentActions()
	if len(rows) != 1 || rows[0].Result != "saved" {
		t.Fatalf("rows: %+v", rows)
	}
	f, err := os.OpenFile(filepath.Join(home, "watch-audit.jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(strings.Repeat("x", 256<<10) + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if rows := recentActions(); len(rows) != 0 {
		t.Fatalf("old rows: %+v", rows)
	}
}

func TestAuditClientRateBuckets(t *testing.T) {
	s := &server{}
	r := httptest.NewRequest("POST", "http://localhost/backends", nil)
	r.RemoteAddr = "192.168.1.3:1"
	for i := 0; i < 10; i++ {
		client := s.actionClient(&accessWriter{email: "Person@Example.COM"}, r)
		if ok, _ := s.allowAction(client); !ok {
			t.Fatalf("request %d refused", i)
		}
	}
	if ok, first := s.allowAction(s.actionClient(&accessWriter{email: "person@example.com"}, r)); ok || !first {
		t.Fatal("case variant did not share bucket")
	}
	if ok, _ := s.allowAction(s.actionClient(&accessWriter{email: "other@example.com"}, r)); !ok {
		t.Fatal("other email shared bucket")
	}
	if ok, _ := s.allowAction("unverified"); !ok {
		t.Fatal("unverified shared email bucket")
	}
	if ok, _ := s.allowAction("loopback"); !ok {
		t.Fatal("loopback shared unverified bucket")
	}
}

func TestAuditRemoteLocalhostAndForwarded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	h := New(context.Background(), Options{Key: "key"})
	for i := 0; i < 10; i++ {
		w := backendRequest(h, "POST", "/backends", "192.168.1.3:1", "localhost", "", "", `{}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("remote %d: %d", i, w.Code)
		}
	}
	local := backendRequest(h, "POST", "/backends", "127.0.0.1:1", "localhost", "key", "", `{}`)
	if local.Code != http.StatusNoContent {
		t.Fatalf("local: %d", local.Code)
	}
	r := httptest.NewRequest("POST", "http://localhost/backends", strings.NewReader(`{}`))
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("X-Forwarded-For", "192.0.2.1")
	r.Header.Set("X-Hrn-Watch-Key", "key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("forwarded: %d", w.Code)
	}
	remote := backendRequest(h, "GET", "/", "192.168.1.3:1", "localhost", "", "", "")
	if strings.Contains(remote.Body.String(), "Recent changes") {
		t.Fatal("remote sees recent changes")
	}
	data, err := os.ReadFile(filepath.Join(home, "watch-audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"client":"unverified"`) {
		t.Fatalf("audit: %s", data)
	}
}

func TestAuditSaveFailureOutcome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions require Unix")
	}
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "backends.json"), []byte(`{"enabled":["codex"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "watch-audit.jsonl"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(home, 0700)
	probe := filepath.Join(home, "permission-probe")
	if f, err := os.Create(probe); err == nil {
		f.Close()
		os.Remove(probe)
		t.Skip("directory permissions not enforced")
	}
	r := httptest.NewRequest("POST", "http://localhost/backends", strings.NewReader(`{"enabled":["claude"]}`))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	(&server{}).saveBackends(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: %d", w.Code)
	}
	rows := recentActions()
	if len(rows) != 1 || rows[0].Result != "error: backend preferences could not be saved" {
		t.Fatalf("rows: %+v", rows)
	}
}

func TestAuditRateLimitExpiryAndSteadyTraffic(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	s := &server{}
	r := httptest.NewRequest(http.MethodPost, "http://localhost/backends", nil)
	refuse := func() { s.refuseAction(httptest.NewRecorder(), r, "client", "test", "test", http.StatusForbidden) }
	for i := 0; i < 10; i++ {
		if ok, _ := s.allowAction("client"); !ok {
			t.Fatal("initial request refused")
		}
	}
	for i := 0; i < 20; i++ {
		refuse()
	}
	for i := range s.rates["client"] {
		s.rates["client"][i] = s.rates["client"][i].Add(-61 * time.Second)
	}
	if ok, _ := s.allowAction("client"); !ok {
		t.Fatal("request refused after expiry")
	}
	for i := 0; i < 9; i++ {
		if ok, _ := s.allowAction("client"); !ok {
			t.Fatal("new bucket not filled")
		}
	}
	for i := 0; i < 20; i++ {
		refuse()
	}
	data, err := os.ReadFile(filepath.Join(home, "watch-audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), `"reason":"rate limit"`); got != 2 {
		t.Fatalf("rate limit lines: %d", got)
	}
	// A client that always keeps a request in the window still gets a line once the window turns over.
	for minute := 0; minute < 2; minute++ {
		for i := 0; i < 10; i++ {
			s.rates["client"][0] = time.Now().Add(-61 * time.Second)
			if ok, _ := s.allowAction("client"); !ok {
				t.Fatalf("minute %d request %d refused", minute, i)
			}
			refuse()
		}
	}
	data, err = os.ReadFile(filepath.Join(home, "watch-audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), `"reason":"rate limit"`) - 2; got < 1 || got > 2 {
		t.Fatalf("steady over-limit traffic wrote %d rate limit lines in two windows, want 1 or 2", got)
	}
}

func TestBackendActionOutcomeAuditFailureStillSaves(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	original := auditAppender
	calls := 0
	auditAppender = func(row actionRow) error {
		calls++
		if calls == 2 {
			return errors.New("outcome append failed")
		}
		return original(row)
	}
	t.Cleanup(func() { auditAppender = original })
	r := httptest.NewRequest(http.MethodPost, "http://localhost/backends", strings.NewReader(`{"enabled":["claude"],"default":"claude"}`))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	(&server{}).saveBackends(w, r)
	if w.Code != http.StatusNoContent || calls != 2 {
		t.Fatalf("status: %d, append calls: %d", w.Code, calls)
	}
	got, err := prefs.Load(home)
	if err != nil || got.Default() != "claude" || !got.Enabled("claude") {
		t.Fatalf("preferences: %+v, %v", got, err)
	}
	data, err := os.ReadFile(filepath.Join(home, "watch-audit.jsonl"))
	if err != nil || strings.Count(strings.TrimSpace(string(data)), "\n") != 0 || !strings.Contains(string(data), `"result":"intent"`) {
		t.Fatalf("audit: %q, %v", data, err)
	}
}
