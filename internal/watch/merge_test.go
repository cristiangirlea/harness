package watch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
)

func mergeFixture(t *testing.T) (http.Handler, batch.Result, string) {
	t.Helper()
	t.Setenv("HRN_HOME", t.TempDir())
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		b, e := c.CombinedOutput()
		if e != nil {
			t.Fatalf("git: %v: %s", e, b)
		}
		return strings.TrimSpace(string(b))
	}
	git("init", "-q")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "base"), []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-qm", "base")
	git("checkout", "-qb", "task")
	if err := os.WriteFile(filepath.Join(dir, "change"), []byte("change"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-qm", "change")
	head := git("rev-parse", "HEAD")
	git("checkout", "-q", "-")
	res := batch.Result{ID: "20261010-120000-abcdef", Dir: dir, Started: time.Now(), Tasks: []batch.TaskResult{{ID: "one", Branch: "task", Commit: head, Status: batch.StatusPass}}}
	saveBatch(t, res)
	return New(context.Background(), Options{Key: "key"}), res, head
}

func TestMergeConfirmation(t *testing.T) {
	h, res, head := mergeFixture(t)
	path := "/batch/" + res.ID + "/merge/one"
	post := func(p, body string) *httptest.ResponseRecorder {
		return backendRequest(h, "POST", p, "127.0.0.1:1", "localhost", "key", "", body)
	}
	ask := post(path, "")
	if ask.Code != 200 {
		t.Fatalf("ask: %d %s", ask.Code, ask.Body)
	}
	var grant struct {
		Token string `json:"token"`
		Head  string `json:"head"`
	}
	if json.Unmarshal(ask.Body.Bytes(), &grant) != nil || grant.Token == "" || grant.Head != head {
		t.Fatalf("grant: %+v", grant)
	}
	body, _ := json.Marshal(map[string]string{"token": grant.Token})
	confirmed := post(path+"/confirm", string(body))
	if confirmed.Code != 200 {
		t.Fatalf("confirm: %d %s", confirmed.Code, confirmed.Body)
	}
	if again := post(path+"/confirm", string(body)); again.Code == 200 {
		t.Fatal("reused token")
	}
	data, err := os.ReadFile(filepath.Join(os.Getenv("HRN_HOME"), "watch-audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), grant.Token) {
		t.Fatal("token in audit")
	}
}

func TestMergeRefusals(t *testing.T) {
	for _, scenario := range []string{"without ask", "moved", "target changed", "failed", "forwarded", "lan", "wrong target"} {
		t.Run(scenario, func(t *testing.T) {
			h, res, _ := mergeFixture(t)
			path := "/batch/" + res.ID + "/merge/one"
			post := func(p, body string) *httptest.ResponseRecorder {
				return backendRequest(h, "POST", p, "127.0.0.1:1", "localhost", "key", "", body)
			}
			var token string
			if scenario != "without ask" && scenario != "failed" && scenario != "forwarded" && scenario != "lan" {
				w := post(path, "")
				var v struct{ Token string }
				if json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Token == "" {
					t.Fatalf("ask: %d %s", w.Code, w.Body)
				}
				token = v.Token
			}
			switch scenario {
			case "moved":
				c := exec.Command("git", "-C", res.Dir, "update-ref", "refs/heads/task", "HEAD")
				if b, e := c.CombinedOutput(); e != nil {
					t.Fatalf("move: %v %s", e, b)
				}
			case "target changed":
				c := exec.Command("git", "-C", res.Dir, "checkout", "-qb", "different-target")
				if b, e := c.CombinedOutput(); e != nil {
					t.Fatalf("change target: %v %s", e, b)
				}
			case "failed":
				res.Tasks[0].Status = batch.StatusFail
				saveBatch(t, res)
			}
			body, _ := json.Marshal(map[string]string{"token": token})
			var w *httptest.ResponseRecorder
			switch scenario {
			case "failed":
				w = post(path, "")
			case "forwarded":
				r := httptest.NewRequest("POST", "http://localhost"+path, nil)
				r.RemoteAddr = "127.0.0.1:1"
				r.Header.Set("X-Hrn-Watch-Key", "key")
				r.Header.Set("X-Forwarded-For", "192.0.2.1")
				w = httptest.NewRecorder()
				h.ServeHTTP(w, r)
			case "lan":
				w = backendRequest(h, "POST", path, "192.0.2.2:1", "localhost", "key", "", "")
			case "wrong target":
				w = post("/batch/"+res.ID+"/merge/other/confirm", string(body))
			default:
				w = post(path+"/confirm", string(body))
			}
			if w.Code < 400 {
				t.Fatalf("accepted: %d %s", w.Code, w.Body)
			}
			if scenario == "moved" && !strings.Contains(w.Body.String(), "branch moved") {
				t.Fatalf("wrong reason: %s", w.Body)
			}
			audit, e := os.ReadFile(filepath.Join(os.Getenv("HRN_HOME"), "watch-audit.jsonl"))
			if e != nil {
				t.Fatal(e)
			}
			if !strings.Contains(string(audit), "refused") && scenario != "moved" && scenario != "target changed" {
				t.Fatalf("no refusal: %s", audit)
			}
			if token != "" && strings.Contains(string(audit), token) {
				t.Fatal("token in audit")
			}
		})
	}
}

func TestMergeExpiredAndBusy(t *testing.T) {
	_, res, _ := mergeFixture(t)
	now := time.Now()
	s := &server{clock: func() time.Time { return now }}
	call := func(fn func(http.ResponseWriter, *http.Request), task, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://localhost/batch/"+res.ID+"/merge/"+task, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:1"
		r.SetPathValue("id", res.ID)
		r.SetPathValue("task", task)
		w := httptest.NewRecorder()
		fn(w, r)
		return w
	}
	ask := func() string {
		w := call(s.askMerge, "one", "")
		var v struct{ Token string }
		if json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Token == "" {
			t.Fatalf("ask: %s", w.Body)
		}
		b, _ := json.Marshal(map[string]string{"token": v.Token})
		return string(b)
	}
	body := ask()
	now = now.Add(2 * time.Minute)
	if w := call(s.confirmMerge, "one", body); w.Code < 400 {
		t.Fatal("expired token accepted")
	}
	body = ask()
	s.mergeBusy = map[string]bool{res.ID: true}
	if w := call(s.confirmMerge, "one", body); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "busy") {
		t.Fatalf("busy: %d %s", w.Code, w.Body)
	}
	s.mergeBusy = nil
	if w := call(s.confirmMerge, "one", body); w.Code < 400 {
		t.Fatal("busy token reused")
	}
}

func TestConcurrentConfirmationsAndForeignClient(t *testing.T) {
	_, res, _ := mergeFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	s := &server{mergeRun: func(ctx context.Context, res batch.Result, task, head string) ([]batch.MergeResult, error) {
		close(entered)
		<-release
		return batch.MergeAt(ctx, res, task, head)
	}}
	request := func(body string) *http.Request {
		r := httptest.NewRequest("POST", "http://localhost/batch/"+res.ID+"/merge/one", strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:1"
		r.SetPathValue("id", res.ID)
		r.SetPathValue("task", "one")
		return r
	}
	ask := func() string {
		w := httptest.NewRecorder()
		s.askMerge(w, request(""))
		var v struct{ Token string }
		if json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Token == "" {
			t.Fatalf("ask: %d %s", w.Code, w.Body)
		}
		b, _ := json.Marshal(map[string]string{"token": v.Token})
		return string(b)
	}
	first, second := ask(), ask()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { w := httptest.NewRecorder(); s.confirmMerge(w, request(first)); done <- w }()
	<-entered
	w := httptest.NewRecorder()
	s.confirmMerge(w, request(second))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "busy") {
		t.Errorf("second: %d %s", w.Code, w.Body)
	}
	close(release)
	if w = <-done; w.Code != 200 {
		t.Fatalf("first: %d %s", w.Code, w.Body)
	}
	third := ask()
	s.opt.Public = &PublicAccess{}
	foreign := &accessWriter{ResponseWriter: httptest.NewRecorder(), email: "other@example.com"}
	s.confirmMerge(foreign, request(third))
	if foreign.ResponseWriter.(*httptest.ResponseRecorder).Code < 400 {
		t.Fatal("foreign client accepted")
	}
	s.opt.Public = nil
	w = httptest.NewRecorder()
	s.confirmMerge(w, request(third))
	if w.Code < 400 {
		t.Fatal("foreign attempt did not consume token")
	}
}

func TestMergePageAndAccessLog(t *testing.T) {
	h, res, _ := mergeFixture(t)
	path := "/batch/" + res.ID
	local := backendRequest(h, "GET", path, "127.0.0.1:1", "localhost", "", "", "")
	remote := backendRequest(h, "GET", path, "192.0.2.4:1", "localhost", "", "", "")
	if !strings.Contains(local.Body.String(), "Merge one") || strings.Contains(remote.Body.String(), "Merge one") {
		t.Fatal("merge button visibility")
	}
	s := &server{opt: Options{Public: &PublicAccess{}}}
	handler := s.publicLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(*accessWriter).email = "owner@example.com"
		r.SetPathValue("id", res.ID)
		r.SetPathValue("task", "one")
		if strings.HasSuffix(r.URL.Path, "/confirm") {
			s.confirmMerge(w, r)
		} else {
			s.askMerge(w, r)
		}
	}))
	request := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "https://localhost"+path, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:1"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	w := request(path+"/merge/one", "")
	var v struct{ Token string }
	if json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Token == "" {
		t.Fatalf("ask: %d %s", w.Code, w.Body)
	}
	b, _ := json.Marshal(map[string]string{"token": v.Token})
	w = request(path+"/merge/one/confirm", string(b))
	if w.Code != 200 {
		t.Fatalf("confirm: %d %s", w.Code, w.Body)
	}
	for _, name := range []string{"watch-access.log", "watch-audit.jsonl"} {
		data, e := os.ReadFile(filepath.Join(os.Getenv("HRN_HOME"), name))
		if e != nil {
			t.Fatal(e)
		}
		if strings.Contains(string(data), v.Token) {
			t.Fatalf("token in %s", name)
		}
	}
}
