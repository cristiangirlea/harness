package watch

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/flow"
	"github.com/cristiangirlea/harness/internal/owner"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestAdaAnswersRecordedBatchQuestions(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:test")
	t.Setenv("ADA_TEST_SECRET", "hidden-gate-value")
	id := "20261022-120000-aabbcc"
	saveBatch(t, batch.Result{ID: id, Owner: owner.Local(), Started: time.Now().Add(-time.Minute), Duration: time.Minute, Tasks: []batch.TaskResult{
		{ID: "broken", Status: batch.StatusFail, Attempts: []batch.Attempt{{Result: agent.Result{Status: agent.StatusOK}, Gates: []batch.GateResult{{Command: "go vet", ExitCode: 1, Output: "API_TOKEN=hidden-gate-value"}}}}},
		{ID: "ready", Status: batch.StatusPass, Commit: "abc"},
	}})
	srv := httptest.NewServer(New(t.Context(), Options{}))
	defer srv.Close()
	for _, tc := range []struct {
		q    string
		want []string
	}{
		{"why did it fail", []string{id, "go vet", "exited 1"}},
		{"why did the last one fail", []string{id, "go vet"}},
		{"why did broken fail", []string{id, "go vet"}},
		{"what happens next", []string{id, "review the diff", "ready"}},
		{"what's next", []string{id, "review the diff"}},
		{"how long did it take", []string{id, "minute"}},
		{"what went wrong today", []string{id, "1", "go vet"}},
	} {
		t.Run(tc.q, func(t *testing.T) {
			a := getAnswer(t, srv.URL, tc.q)
			if a.Ref != id || a.URL != "/flow/"+id {
				t.Fatalf("reference = %+v", a)
			}
			for _, want := range tc.want {
				if !strings.Contains(a.Text, want) {
					t.Errorf("%q missing %q", a.Text, want)
				}
			}
			if strings.Contains(a.Text, "hidden-gate-value") {
				t.Fatal("gate secret in answer")
			}
			if len([]rune(a.Text)) > 200 {
				t.Fatalf("answer too long: %s", a.Text)
			}
		})
	}
	for _, q := range []string{"tell me a joke", "why did absent fail"} {
		a := getAnswer(t, srv.URL, q)
		if a.Ref != "" || a.URL != "" || a.Text != "I can only answer from what hrn recorded: why something failed, what it is doing, what happens next, how long it took." {
			t.Fatalf("fallback = %+v", a)
		}
	}
}

func TestAdaAnswersCurrentRunStep(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:test")
	id := saveRun(t, "fake", "inspect widgets", nil)
	appendEvent(t, filepath.Join(runlog.Root(), id, "events.jsonl"), agent.Event{Time: time.Now(), Kind: agent.EventToolCall, Tool: "Read", Text: "widgets.go"})
	srv := httptest.NewServer(New(t.Context(), Options{}))
	defer srv.Close()
	for _, q := range []string{"what is it doing", "where is it", "what is widgets doing"} {
		a := getAnswer(t, srv.URL, q)
		if a.Ref != id || !strings.Contains(a.Text, id) || !strings.Contains(a.Text, "Read") {
			t.Fatalf("current step: %+v", a)
		}
	}
}

func TestAdaAnswersNameNothingForAStranger(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:test")
	f := newAccessFixture(t)
	id := saveRun(t, "fake", "private zebra", &agent.Result{Status: agent.StatusError, Error: "private cause"})
	saveBatch(t, batch.Result{ID: "20261022-120000-aabbcc", Owner: owner.Local(), Tasks: []batch.TaskResult{{ID: "private task", Status: batch.StatusFail}}})
	f.opt.OwnerEmails = []string{"owner@example.com"}
	h := New(t.Context(), f.opt)
	for _, q := range []string{"why did it fail", "why did " + id + " fail", "what is it doing", "what happens next", "how long did it take", "what went wrong today"} {
		resp := f.request(h, "/bot/answer?q="+url.QueryEscape(q), "harness.example.com", "", f.tokenFor("stranger@example.com"))
		var a struct{ Text, Ref, URL string }
		if resp.Code != 200 || json.Unmarshal(resp.Body.Bytes(), &a) != nil {
			t.Fatalf("answer: %d %s", resp.Code, resp.Body)
		}
		if a.Ref != "" || a.URL != "" || strings.Contains(a.Text, id) || strings.Contains(a.Text, "aabbcc") || strings.Contains(a.Text, "private") {
			t.Fatalf("private answer: %+v", a)
		}
	}
}

func TestAdaAnswersSelectFailuresAndRecordedDurations(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:test")
	failed := saveRun(t, "fake", "repair widgets", &agent.Result{Status: agent.StatusError, Error: "widget check failed", Duration: 42 * time.Second})
	passed := saveRun(t, "fake", "inspect widgets", &agent.Result{Status: agent.StatusOK, Duration: 3 * time.Second})
	failedRecord, err := runlog.Load(failed)
	if err != nil {
		t.Fatal(err)
	}
	passedRecord, err := runlog.Load(passed)
	if err != nil {
		t.Fatal(err)
	}
	failedRecord.Started = passedRecord.Started.Add(-time.Second)
	data, err := json.Marshal(failedRecord)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runlog.Root(), failed, "result.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(t.Context(), Options{Runs: 1}))
	defer srv.Close()
	for _, q := range []string{"why did it fail", "why did repair fail", "why did " + failed + " fail"} {
		a := getAnswer(t, srv.URL, q)
		if a.Ref != failed || !strings.Contains(a.Text, "widget check failed") {
			t.Fatalf("failure: %+v", a)
		}
	}
	for _, tc := range []struct{ q, id, duration string }{
		{"how long did it take", passed, "three seconds"},
		{"how long did repair take", failed, "42 seconds"},
	} {
		a := getAnswer(t, srv.URL, tc.q)
		if a.Ref != tc.id || !strings.Contains(a.Text, tc.duration) {
			t.Fatalf("duration: %+v", a)
		}
	}
}

func TestAdaAnswersUseTheLastFailedAttempt(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:test")
	id := "20261022-120000-aabbcc"
	old := batch.Attempt{Result: agent.Result{Status: agent.StatusOK}, Gates: []batch.GateResult{{Command: "old-gate", ExitCode: 1}}}
	saveBatch(t, batch.Result{ID: id, Started: time.Now().Add(-time.Minute), Duration: time.Minute, Tasks: []batch.TaskResult{
		{ID: "recovered", Status: batch.StatusPass, Attempts: []batch.Attempt{old, {Result: agent.Result{Status: agent.StatusOK}}}},
		{ID: "broken", Status: batch.StatusFail, Attempts: []batch.Attempt{old, {Result: agent.Result{Status: agent.StatusError, Error: "final error"}}}},
	}})
	srv := httptest.NewServer(New(t.Context(), Options{}))
	defer srv.Close()
	a := getAnswer(t, srv.URL, "why did it fail")
	if a.Ref != id || !strings.Contains(a.Text, "final error") || strings.Contains(a.Text, "old-gate") || strings.Contains(a.Text, "recovered") {
		t.Fatalf("last attempt: %+v", a)
	}
}

func TestAdaAnswersUseOpenStepsAndRecordedWaitingReasons(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:test")
	id := "20261022-120000-aabbcc"
	now := time.Now()
	saveBatch(t, batch.Result{ID: id, Started: now, Tasks: []batch.TaskResult{{ID: "work", Status: batch.StatusRunning}}})
	w, err := flow.Open(batch.FlowPath(id), flow.Event{Batch: id, Owner: owner.Local()})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []flow.Event{
		{Time: now, Step: "batch", Kind: flow.KindBatch, Status: flow.StatusStarted},
		{Time: now.Add(time.Second), Step: "open", Parent: "batch", Kind: flow.KindGate, Name: "approval", Status: flow.StatusWaiting, Reason: "owner approval"},
		{Time: now.Add(2 * time.Second), Step: "closed", Parent: "batch", Kind: flow.KindTool, Name: "finished-tool", Status: flow.StatusStarted},
		{Time: now.Add(3 * time.Second), Step: "closed", Parent: "batch", Kind: flow.KindTool, Name: "finished-tool", Status: flow.StatusOK},
	} {
		w.Emit(e)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(t.Context(), Options{}))
	defer srv.Close()
	a := getAnswer(t, srv.URL, "what is it doing")
	if a.Ref != id || !strings.Contains(a.Text, "approval") || strings.Contains(a.Text, "finished-tool") {
		t.Fatalf("open step: %+v", a)
	}
	a = getAnswer(t, srv.URL, "what happens next")
	if a.Ref != id || !strings.Contains(a.Text, "owner approval") {
		t.Fatalf("waiting reason: %+v", a)
	}
}

func TestAdaAnswersMaskBeforeShortening(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:test")
	secret := strings.Repeat("s", 250)
	t.Setenv("ADA_TEST_SECRET", secret)
	id := saveRun(t, "fake", "check masking", &agent.Result{Status: agent.StatusError, Error: "API_TOKEN=" + secret + " missing permission"})
	srv := httptest.NewServer(New(t.Context(), Options{}))
	defer srv.Close()
	a := getAnswer(t, srv.URL, "why did it fail")
	if a.Ref != id || strings.Contains(a.Text, strings.Repeat("s", 10)) || !strings.Contains(a.Text, "missing permission") || len([]rune(a.Text)) > 200 {
		t.Fatalf("masking: %+v", a)
	}
}

func TestAdaAnswersCountOnlyTodaysFailures(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:test")
	now := time.Now()
	for _, tc := range []struct {
		id  string
		at  time.Time
		why string
	}{
		{"20261022-120000-aabbcc", now, "latest cause"},
		{"20261022-110000-aabbcc", now.Add(-time.Nanosecond), "other cause"},
		{"20261021-120000-aabbcc", now.AddDate(0, 0, -1), "yesterday cause"},
		{"20261019-120000-aabbcc", now.AddDate(0, 0, -3), "old cause"},
	} {
		saveBatch(t, batch.Result{ID: tc.id, Started: tc.at.Add(-time.Second), Duration: time.Second, Tasks: []batch.TaskResult{{ID: "broken", Status: batch.StatusFail, Error: tc.why}}})
	}
	srv := httptest.NewServer(New(t.Context(), Options{}))
	defer srv.Close()
	// More passing runs than the scan cap sit between today's failures: the count must still be exact.
	for i := 0; i < answerScan+5; i++ {
		saveRun(t, "fake", "fine", &agent.Result{Status: agent.StatusOK})
	}
	a := getAnswer(t, srv.URL, "what went wrong today")
	if a.Ref != "20261022-120000-aabbcc" || !strings.Contains(a.Text, "2 failed records today") || !strings.Contains(a.Text, "latest cause") || strings.Contains(a.Text, "yesterday") {
		t.Fatalf("today: %+v", a)
	}
}

func getAnswer(t *testing.T, base, q string) struct{ Text, Ref, URL string } {
	t.Helper()
	code, body := get(t, base+"/bot/answer?q="+url.QueryEscape(q))
	var a struct{ Text, Ref, URL string }
	if code != 200 || json.Unmarshal([]byte(body), &a) != nil {
		t.Fatalf("answer: %d %s", code, body)
	}
	return a
}

func TestAdaAnswersShortenPathsLikeTheFlowPage(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:test")
	home, _ := os.UserHomeDir()
	full := filepath.Join(home, "work", "repo", "pkg", "file.go")
	id := saveRun(t, "fake", "edit", &agent.Result{Status: agent.StatusError, Error: "cannot write " + full})
	h := New(t.Context(), Options{LAN: []Endpoint{{Addr: "192.168.1.20:8089"}}})
	body := viewAs(t, h, "/bot/answer?q=why+did+it+fail", "192.168.1.20:4567")
	if !strings.Contains(body, id) || strings.Contains(body, filepath.ToSlash(home)) || strings.Contains(strings.ReplaceAll(body, "\\\\", "\\"), home) {
		t.Fatalf("a LAN viewer saw the home path: %s", body)
	}
}

func TestAdaAnswersReadABoundedNumberOfRecords(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:test")
	for i := 0; i < answerScan+5; i++ {
		saveRun(t, "fake", "fine", &agent.Result{Status: agent.StatusOK})
	}
	srv := httptest.NewServer(New(t.Context(), Options{}))
	defer srv.Close()
	start := time.Now()
	if a := getAnswer(t, srv.URL, "why did it fail"); a.Text != answerHelp {
		t.Fatalf("answer = %+v", a)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("a question over many records took %s", took)
	}
}
