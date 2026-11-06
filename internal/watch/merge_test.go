package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/ws"
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
			if scenario != "without ask" && scenario != "forwarded" && scenario != "lan" {
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
			if !strings.Contains(string(audit), "refused") && scenario != "moved" && scenario != "target changed" && scenario != "failed" {
				t.Fatalf("no refusal: %s", audit)
			}
			if scenario == "failed" && !strings.Contains(string(audit), "task is no longer eligible") {
				t.Fatalf("missing failed outcome: %s", audit)
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
	s.mergeBusy = map[string]bool{filepath.Clean(res.Dir): true}
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
	first := ask()
	secondRes := res
	secondRes.ID = "20261010-120001-abcdef"
	saveBatch(t, secondRes)
	res = secondRes
	second := ask()
	res.ID = "20261010-120000-abcdef"
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { w := httptest.NewRecorder(); s.confirmMerge(w, request(first)); done <- w }()
	<-entered
	w := httptest.NewRecorder()
	res.ID = secondRes.ID
	s.confirmMerge(w, request(second))
	res.ID = "20261010-120000-abcdef"
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "busy") {
		t.Errorf("second: %d %s", w.Code, w.Body)
	}
	close(release)
	if w = <-done; w.Code != 200 {
		t.Fatalf("first: %d %s", w.Code, w.Body)
	}
	s.mergeRun = nil
	third := ask()
	s.opt.Public = &PublicAccess{}
	foreign := &accessWriter{ResponseWriter: httptest.NewRecorder(), email: "other@example.com"}
	foreignRequest := request(third)
	foreignRequest = foreignRequest.WithContext(context.WithValue(foreignRequest.Context(), accessContextKey{}, foreign))
	s.confirmMerge(foreign, foreignRequest)
	if foreign.ResponseWriter.(*httptest.ResponseRecorder).Code < 400 {
		t.Fatal("foreign client accepted")
	}
	s.opt.Public = nil
	w = httptest.NewRecorder()
	s.confirmMerge(w, request(third))
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "invalid confirmation") {
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

func TestMergeRouteGuards(t *testing.T) {
	h, res, _ := mergeFixture(t)
	path := "/batch/" + res.ID + "/merge/one"
	for _, suffix := range []string{"", "/confirm"} {
		for _, headers := range []map[string]string{{}, {"X-Hrn-Watch-Key": "key", "Origin": "http://evil.example"}, {"X-Hrn-Watch-Key": "key", "Sec-Fetch-Site": "cross-site"}} {
			r := httptest.NewRequest("POST", "http://localhost"+path+suffix, strings.NewReader(`{}`))
			r.RemoteAddr = "127.0.0.1:1"
			for k, v := range headers {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("%s %v: %d %s", suffix, headers, w.Code, w.Body)
			}
		}
	}
}

func TestPublicMergeOwnerOnly(t *testing.T) {
	f := newAccessFixture(t)
	_, res, _ := mergeFixture(t)
	t.Setenv("HRN_OWNER", "local:me")
	data, _ := json.Marshal([]map[string]string{{"viewer": "email:reader@example.com", "project": res.Dir}})
	if err := os.WriteFile(filepath.Join(os.Getenv("HRN_HOME"), "grants.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	f.opt.OwnerEmails = []string{"owner@example.com"}
	f.opt.Key = "key"
	h := New(context.Background(), f.opt)
	path := "/batch/" + res.ID
	reader := f.tokenFor("reader@example.com")
	owner := f.tokenFor("owner@example.com")
	page := f.request(h, path, "harness.example.com", "", reader)
	if page.Code != 200 || strings.Contains(page.Body.String(), "Merge one") {
		t.Fatalf("reader page: %d %s", page.Code, page.Body)
	}
	post := func(token, suffix, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "https://harness.example.com"+path+"/merge/one"+suffix, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("Cf-Connecting-Ip", "198.51.100.8")
		r.Header.Set("Cf-Access-Jwt-Assertion", token)
		r.Header.Set("X-Hrn-Watch-Key", "key")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := post(reader, "", ""); w.Code != 403 {
		t.Fatalf("reader ask: %d %s", w.Code, w.Body)
	}
	w := post(owner, "", "")
	var grant struct{ Token string }
	json.Unmarshal(w.Body.Bytes(), &grant)
	if w.Code != 200 || grant.Token == "" {
		t.Fatalf("owner ask: %d %s", w.Code, w.Body)
	}
	body, _ := json.Marshal(map[string]string{"token": grant.Token})
	if w = post(reader, "/confirm", string(body)); w.Code != 403 {
		t.Fatalf("reader confirm: %d %s", w.Code, w.Body)
	}
	if w = post(owner, "/confirm", string(body)); w.Code != 200 {
		t.Fatalf("owner confirm: %d %s", w.Code, w.Body)
	}
}

func TestOwnedMergeOutlivesDisconnect(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "unknown"}[unknown], func(t *testing.T) {
			_, res, head := mergeFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := &server{mergeRun: func(owned context.Context, res batch.Result, task, head string) ([]batch.MergeResult, error) {
				if out, err := exec.CommandContext(owned, "git", "-C", res.Dir, "merge", "--no-ff", "--no-edit", head).CombinedOutput(); err != nil {
					return nil, fmt.Errorf("merge: %w: %s", err, out)
				}
				cancel()
				ref := "HEAD"
				if unknown {
					ref = "refs/heads/missing-receipt"
				}
				receipt, err := exec.CommandContext(owned, "git", "-C", res.Dir, "rev-parse", "--verify", ref).Output()
				if owned.Err() != nil {
					return nil, owned.Err()
				}
				if err != nil {
					return []batch.MergeResult{{Task: task, Error: batch.ErrMergedUnconfirmed.Error()}}, batch.ErrMergedUnconfirmed
				}
				return []batch.MergeResult{{Task: task, Commit: strings.TrimSpace(string(receipt))}}, nil
			}}
			request := func(body string) *http.Request {
				r := httptest.NewRequest("POST", "http://localhost/batch/"+res.ID+"/merge/one", strings.NewReader(body)).WithContext(ctx)
				r.RemoteAddr = "127.0.0.1:1"
				r.SetPathValue("id", res.ID)
				r.SetPathValue("task", "one")
				return r
			}
			ask := httptest.NewRecorder()
			s.askMerge(ask, request(""))
			var grant struct{ Token string }
			if err := json.Unmarshal(ask.Body.Bytes(), &grant); err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(map[string]string{"token": grant.Token})
			w := httptest.NewRecorder()
			s.confirmMerge(w, request(string(body)))
			if w.Code != 200 {
				t.Fatalf("confirm: %d %s", w.Code, w.Body)
			}
			var response map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			want := "merged, commit unknown"
			if !unknown {
				commit := response["commit"]
				if commit == "" {
					t.Fatal("missing receipt")
				}
				b, err := exec.Command("git", "-C", res.Dir, "rev-parse", commit+"^2").Output()
				if err != nil || strings.TrimSpace(string(b)) != head {
					t.Fatalf("wrong receipt: %s %v", b, err)
				}
				want = "merged " + commit
			} else if response["outcome"] != want {
				t.Fatalf("response=%v", response)
			}
			audit, err := os.ReadFile(filepath.Join(os.Getenv("HRN_HOME"), "watch-audit.jsonl"))
			if err != nil || !strings.Contains(string(audit), want) || strings.Contains(string(audit), "merge failed") {
				t.Fatalf("audit=%s err=%v", audit, err)
			}
		})
	}
}

func TestMergePageUnknownOutcome(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	script := `
const vm = require('node:vm');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const source = fs.readFileSync(0, 'utf8');
(async () => {
 for (const scenario of ['reject', 'json', 'server', 'forbidden', 'busy', 'success', 'unknown']) {
  const elements = {};
  for (const id of ['merges', 'merge-status', 'merge-confirm', 'merge-cancel']) {
   elements[id] = {dataset: {batch: 'batch', key: 'key'}, addEventListener(event, fn) { this[event] = fn; }};
  }
  let calls = 0;
  vm.runInNewContext(source, {
   document: {getElementById: id => elements[id], querySelectorAll: () => [], querySelector: () => null},
   window: {}, setInterval() {},
   fetch: async () => {
    if (++calls === 1) return {ok: true, json: async () => ({task:'one', branch:'task', head:'abcdef', target:'main', token:'token'})};
    if (scenario === 'reject') throw new Error('raw network error');
    return {ok: !['forbidden','busy','server'].includes(scenario), status: scenario === 'forbidden' ? 403 : scenario === 'busy' ? 409 : scenario === 'server' ? 503 : 200,
     text: async () => scenario === 'forbidden' ? 'invalid or expired confirmation' : 'batch is busy', json: async () => {
      if (scenario === 'json') throw new Error('raw JSON error');
      return scenario === 'unknown' ? {outcome:'merged, commit unknown'} : {commit:'abc123'};
     }};
   }
  });
  await elements.merges.click({target: {dataset: {mergeTask:'one'}}});
  await elements['merge-confirm'].click();
  const want = {forbidden:'invalid or expired confirmation', busy:'batch is busy', success:'Merged: abc123', unknown:'merged, commit unknown'}[scenario] || 'Outcome unknown: reload the batch page to see whether it merged.';
  assert.equal(elements['merge-status'].textContent, want, scenario);
 }
})().catch(error => { console.error(error); process.exitCode = 1; });`
	cmd := exec.CommandContext(ctx, node, "-e", script)
	cmd.Stdin = strings.NewReader(mustAsset("assets/watch.js"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("page: %v\n%s", err, out)
	}
}

func TestMergeSharedLockBusy(t *testing.T) {
	h, res, _ := mergeFixture(t)
	release, busy, err := ws.LockFile(filepath.Join(res.Dir, ".git", "hrn-merge.lock"), "CLI merger")
	if err != nil || busy != "" {
		t.Fatalf("busy=%s err=%v", busy, err)
	}
	defer release()
	path := "/batch/" + res.ID + "/merge/one"
	ask := backendRequest(h, "POST", path, "127.0.0.1:1", "localhost", "key", "", "")
	var grant struct{ Token string }
	if err := json.Unmarshal(ask.Body.Bytes(), &grant); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"token": grant.Token})
	w := backendRequest(h, "POST", path+"/confirm", "127.0.0.1:1", "localhost", "key", "", string(body))
	if w.Code != 409 || strings.TrimSpace(w.Body.String()) != "batch is busy" {
		t.Fatalf("confirm: %d %s", w.Code, w.Body)
	}
	audit, err := os.ReadFile(filepath.Join(os.Getenv("HRN_HOME"), "watch-audit.jsonl"))
	if err != nil || !strings.Contains(string(audit), "batch is busy") || strings.Contains(string(audit), "merge failed") {
		t.Fatalf("audit=%s err=%v", audit, err)
	}
}
