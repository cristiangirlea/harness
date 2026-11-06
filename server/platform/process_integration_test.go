package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkerHelperProcess(t *testing.T) {
	if os.Getenv("HRN_WORKER_HELPER") != "1" {
		return
	}
	worker := &Worker{HRNPath: os.Getenv("HRN_WORKER_HRN"), ControlURL: os.Getenv("HRN_WORKER_CONTROL"),
		ProjectID: os.Getenv("HRN_WORKER_PROJECT"), Token: os.Getenv("HRN_WORKER_TOKEN"),
		WorkerID: "killed-worker", WorkspaceRoot: os.Getenv("HRN_WORKER_ROOT"), ModelURL: os.Getenv("HRN_WORKER_MODEL"), ModelKey: "fixture"}
	ran, err := worker.RunOnce(context.Background())
	if err != nil || (!ran && os.Getenv("HRN_WORKER_ALLOW_IDLE") != "1") {
		t.Fatalf("helper worker: %v %v", ran, err)
	}
}

func TestWorkerFaultOracleFollowsTheRecoveryRules(t *testing.T) {
	for _, path := range []string{"/internal/projects/project-a/claim", "/internal/projects/project-b/claim"} {
		if got := workerControlCall("POST", path); got != "POST /internal/projects/{id}/claim" {
			t.Fatalf("normalised call: %s", got)
		}
	}
	if got := workerControlCall("GET", "/internal/projects/p/jobs/j/checkpoint"); got != "GET /internal/projects/{id}/jobs/{id}/checkpoint" {
		t.Fatal(got)
	}
	pending := &Checkpoint{}
	if err := json.Unmarshal([]byte(`{"version":1,"sequence":1,"boundary":"tool_intent","task_digest":"digest","pending":{"kind":"tool","id":"tool-1"},"next_turn":1}`), pending); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		cp   *Checkpoint
		want string
	}{{nil, "resume"}, {&Checkpoint{}, "resume"}, {pending, "fail"}, {&Checkpoint{Finished: true}, "complete"}} {
		if got := workerFaultRecovery(tc.cp); got != tc.want {
			t.Fatalf("recovery for %s = %s, want %s", checkpointSummary(tc.cp), got, tc.want)
		}
	}
}

func TestWorkerFaultModelTurnComesFromMessages(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"messages":[{"role":"user"}]}`, 1},
		{`{"messages":[{"role":"user"},{"role":"assistant"},{"role":"user"}]}`, 2},
		{`{"messages":[{"role":"user"},{"role":"assistant"}]}`, 2},
		{`{"messages":[]}`, 0},
		{`{"messages":[{"role":"assistant"}]}`, 0},
		{`{"messages":[{"role":"user"},{"role":"assistant"},{"role":"assistant"}]}`, 0},
		{`{"messages":[{"role":"user"},{"role":"user"},{"role":"user"}]}`, 0},
		{`{"messages":[{"role":"system"}]}`, 0},
		{`{`, 0},
	} {
		got, err := workerFaultModelTurn(strings.NewReader(tc.body))
		if got != tc.want || (err != nil) != (tc.want == 0) {
			t.Fatalf("model turn = %d, %v; want %d", got, err, tc.want)
		}
	}
}

func TestWorkerFaultCoverageRequiresEveryControlCallKind(t *testing.T) {
	calls := []string{
		"POST /internal/projects/{id}/claim",
		"GET /internal/projects/{id}/jobs/{id}/checkpoint",
		"POST /internal/jobs/{id}/checkpoint",
		"POST /internal/jobs/{id}/finish",
	}
	if err := workerFaultCoverage(calls, unreachableCalls); err != nil {
		t.Fatal(err)
	}
	for i := range calls {
		missing := append(append([]string(nil), calls[:i]...), calls[i+1:]...)
		if workerFaultCoverage(missing, unreachableCalls) == nil {
			t.Fatalf("missing call %q accepted", calls[i])
		}
	}
	for _, excluded := range []map[string]string{nil, {"POST /internal/jobs/{id}/heartbeat": ""}} {
		if workerFaultCoverage(calls, excluded) == nil {
			t.Fatal("missing heartbeat exclusion reason accepted")
		}
	}
	if err := workerFaultCoverage(append(calls, "POST /internal/jobs/{id}/heartbeat"), nil); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerFaultSubtestNamesSelectSingleCases(t *testing.T) {
	got := workerFaultCallName("GET /internal/projects/{id}/jobs/{id}/checkpoint")
	if want := "GET _internal_projects__id__jobs__id__checkpoint"; got != want {
		t.Fatalf("call name = %q, want %q", got, want)
	}
}

func TestWorkerFaultIndicesExcludeHeartbeats(t *testing.T) {
	var run workerFaultRun
	claim := "POST /internal/projects/{id}/claim"
	checkpoint := "GET /internal/projects/{id}/jobs/{id}/checkpoint"
	for _, call := range []string{workerHeartbeatCall, claim, workerHeartbeatCall, checkpoint, workerHeartbeatCall} {
		run.recordCall(call, true)
	}
	run.recordCall("POST /internal/jobs/{id}/finish", false)
	run.recordCall(workerHeartbeatCall, false)
	if len(run.calls) != 2 || run.calls[0] != claim || run.calls[1] != checkpoint || len(run.heartbeats) != 4 {
		t.Fatalf("recorded calls=%v heartbeats=%v", run.calls, run.heartbeats)
	}
}

type workerFault struct {
	call int
	mode string
}

const (
	killBeforeServe     = "killBeforeServe"
	killAfterServe      = "killAfterServe"
	errorReply          = "errorReply"
	workerHeartbeatCall = "POST /internal/jobs/{id}/heartbeat"
)

var unreachableCalls = map[string]string{
	workerHeartbeatCall: "The heartbeat interval is longer than a run; incidental heartbeats are recorded separately and excluded from fault targets.",
}

var workerControlCallKinds = []string{
	"POST /internal/projects/{id}/claim",
	"GET /internal/projects/{id}/jobs/{id}/checkpoint",
	"POST /internal/jobs/{id}/checkpoint",
	workerHeartbeatCall,
	"POST /internal/jobs/{id}/finish",
}

type workerFaultRun struct {
	calls      []string
	heartbeats []string
	recovery   string
}

func (r *workerFaultRun) recordCall(call string, initial bool) {
	if call == workerHeartbeatCall {
		r.heartbeats = append(r.heartbeats, call)
	} else if initial {
		r.calls = append(r.calls, call)
	}
}

func workerFaultCoverage(calls []string, unreachable map[string]string) error {
	present := make(map[string]bool)
	for _, call := range calls {
		present[call] = true
	}
	for _, call := range workerControlCallKinds {
		if !present[call] && strings.TrimSpace(unreachable[call]) == "" {
			return fmt.Errorf("control call %q has no fault coverage or exclusion reason", call)
		}
	}
	return nil
}

func workerFaultCallName(call string) string {
	return strings.NewReplacer("/", "_", "{", "_", "}", "_").Replace(call)
}

func workerFaultModelTurn(body io.Reader) (int, error) {
	var request struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(body).Decode(&request); err != nil {
		return 0, fmt.Errorf("decode model messages: %w", err)
	}
	if len(request.Messages) == 0 || request.Messages[0].Role != "user" {
		return 0, errors.New("model request must start with the user's task")
	}
	// A paused turn continues with the assistant's reply last, so turns follow assistant replies.
	turn := 1
	for i, message := range request.Messages[1:] {
		switch {
		case message.Role == "assistant" && request.Messages[i].Role == "user":
			turn++
		case message.Role == "user" && request.Messages[i].Role == "assistant":
		default:
			return 0, fmt.Errorf("model message %d role = %q after %q", i+1, message.Role, request.Messages[i].Role)
		}
	}
	return turn, nil
}

// checkpointSummary keeps failure output readable; the raw payload holds the whole request.
func checkpointSummary(cp *Checkpoint) string {
	if cp == nil {
		return "none"
	}
	return fmt.Sprintf("seq=%d boundary=%s next_turn=%d pending=%v finished=%v", cp.Sequence, cp.Boundary, cp.NextTurn, cp.Pending != nil, cp.Finished)
}

func workerControlCall(method, path string) string {
	parts := strings.Split(path, "/")
	for i := 1; i+1 < len(parts); i++ {
		if parts[i] == "projects" || parts[i] == "jobs" {
			parts[i+1] = "{id}"
		}
	}
	return method + " " + strings.Join(parts, "/")
}

// workerFaultRecovery mirrors ResolveRecovery: a pending effect needs an operator, a finished run completes.
func workerFaultRecovery(cp *Checkpoint) string {
	switch {
	case cp != nil && cp.Pending != nil:
		return "fail"
	case cp != nil && cp.Finished:
		return "complete"
	default:
		return "resume"
	}
}

func TestPostgresWorkerRecoversFromEveryControlCallFault(t *testing.T) {
	reference := runWorkerWithFault(t, workerFault{})
	if len(reference.calls) == 0 {
		t.Fatal("reference worker made no control calls")
	}
	if err := workerFaultCoverage(reference.calls, unreachableCalls); err != nil {
		t.Fatal(err)
	}
	t.Logf("reference: %d fault targets, %d excluded heartbeats", len(reference.calls), len(reference.heartbeats))
	outcomes := make(map[string]int)
	ran, planned := 0, 0
	for i, call := range reference.calls {
		if reason, unreachable := unreachableCalls[call]; unreachable {
			if reason == "" {
				t.Fatalf("unreachable call %q has no reason", call)
			}
			continue
		}
		for _, mode := range []string{killBeforeServe, killAfterServe, errorReply} {
			planned++
			t.Run(fmt.Sprintf("call%02d-%s-%s", i+1, workerFaultCallName(call), mode), func(t *testing.T) {
				ran++
				observed := runWorkerWithFault(t, workerFault{call: i + 1, mode: mode})
				outcomes[observed.recovery]++
				if len(observed.calls) != i+1 {
					t.Fatalf("fault reached %d calls, want %d", len(observed.calls), i+1)
				}
				for j := range observed.calls {
					if observed.calls[j] != reference.calls[j] {
						t.Fatalf("call %d changed: %q, want %q", j+1, observed.calls[j], reference.calls[j])
					}
				}
			})
		}
	}
	// A filtered run must allow selecting one case without requiring the whole table.
	if ran == planned {
		for _, kind := range []string{"resume", "complete", "recovery_required"} {
			if outcomes[kind] == 0 {
				t.Errorf("fault table did not exercise %s recovery", kind)
			}
		}
	}
}

func runWorkerWithFault(t *testing.T, fault workerFault) workerFaultRun {
	t.Helper()
	store := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	project, token := testProject(t, store)
	job, _, err := store.Submit(ctx, testSubmission(project, "crash-table", "two turns"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dir, err := prepareWorkspace(root, job, false)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "untracked.txt")
	if err := os.WriteFile(marker, []byte("retained after worker death"), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var observed workerFaultRun
	var saved []Checkpoint
	var acknowledged []Checkpoint
	var turn atomic.Int32
	turn.Store(1)
	var modelCalls [3]atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := workerFaultModelTurn(r.Body)
		if err != nil {
			t.Errorf("model request: %v", err)
			http.Error(w, "unexpected model messages", 500)
			return
		}
		if n > 2 {
			t.Errorf("model turn %d beyond the two-turn scenario", n)
			http.Error(w, "unexpected model turn", 500)
			return
		}
		modelCalls[n].Add(1)
		if next := turn.Load(); int(next) != n {
			t.Errorf("model messages identify turn %d, checkpoint next_turn=%d", n, next)
			http.Error(w, "model turn differs from checkpoint", 500)
			return
		}
		if n == 1 {
			fakeTurn(w, "pause", "pause_turn")
		} else {
			fakeTurn(w, "finished after process restart", "end_turn")
		}
	}))
	defer model.Close()
	hit := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var restarting atomic.Bool
	faulted := false
	base := (&HTTPServer{Store: store, OperatorToken: "operator-token-abcdefghijklmnopqrstuvwxyz",
		ScalerToken: "scaler-token-abcdefghijklmnopqrstuvwxyz12"}).Handler()
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		call := workerControlCall(r.Method, r.URL.Path)
		heartbeat := call == workerHeartbeatCall
		initial := !restarting.Load() && !faulted
		observed.recordCall(call, initial)
		crash := initial && !heartbeat && fault.call > 0 && len(observed.calls) == fault.call
		if crash {
			faulted = true
		}
		if crash && fault.mode == killBeforeServe {
			hit <- struct{}{}
			<-release
			return
		}
		var cp *Checkpoint
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/checkpoint") {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
			var request checkpointRequest
			if err := json.Unmarshal(raw, &request); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			cp = &request.Checkpoint
			turn.Store(int32(cp.NextTurn))
		}
		record := httptest.NewRecorder()
		base.ServeHTTP(record, r)
		if cp != nil && record.Code == http.StatusOK {
			persisted, err := store.LatestCheckpoint(r.Context(), project.TenantID, project.ID, job.ID)
			if err != nil || persisted == nil || !bytes.Equal(persisted.Bytes(), cp.Bytes()) {
				t.Errorf("checkpoint acknowledged without durable snapshot: %s %v", checkpointSummary(persisted), err)
				http.Error(w, "checkpoint not durable", 500)
				return
			}
			saved = append(saved, *cp)
		}
		if crash {
			hit <- struct{}{}
			if fault.mode == errorReply {
				http.Error(w, "injected error after control service commit", http.StatusInternalServerError)
				return
			}
			<-release
			return
		}
		for key, values := range record.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(record.Code)
		_, _ = w.Write(record.Body.Bytes())
		if cp != nil && record.Code == http.StatusOK {
			acknowledged = append(acknowledged, *cp)
		}
	}))
	defer control.Close()
	defer unblock()
	if admitted, err := store.AdmitOne(ctx); err != nil || admitted != job.ID {
		t.Fatalf("admit: %s %v", admitted, err)
	}
	binary := realHRN(t)
	start := func() (*exec.Cmd, <-chan error, *bytes.Buffer) {
		child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWorkerHelperProcess$")
		child.Env = append(os.Environ(), "HRN_WORKER_HELPER=1", "HRN_WORKER_ALLOW_IDLE=1",
			"HRN_WORKER_HRN="+binary, "HRN_WORKER_CONTROL="+control.URL,
			"HRN_WORKER_MODEL="+model.URL, "HRN_WORKER_PROJECT="+project.ID,
			"HRN_WORKER_TOKEN="+token, "HRN_WORKER_ROOT="+root)
		stderr := new(bytes.Buffer)
		child.Stderr = stderr
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- child.Wait() }()
		t.Cleanup(func() { _ = child.Process.Kill() })
		return child, done, stderr
	}
	child, done, stderr := start()
	want := Succeeded
	if fault.call > 0 {
		if fault.mode == errorReply {
			select {
			case <-done:
				if ctx.Err() != nil {
					t.Fatal("error-reply worker did not exit on its own")
				}
			case <-ctx.Done():
				t.Fatal("error-reply worker did not exit on its own")
			}
			select {
			case <-hit:
			default:
				t.Fatalf("worker exited before error reply: %s", stderr.String())
			}
		} else {
			select {
			case <-hit:
			case err := <-done:
				t.Fatalf("worker exited before fault: %v %s", err, stderr.String())
			case <-ctx.Done():
				t.Fatal("worker never reached fault")
			}
			if err := child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err == nil {
				t.Fatal("fault worker unexpectedly exited cleanly")
			}
		}
		restarting.Store(true)
		unblock()
		mu.Lock()
		before := append([]Checkpoint(nil), saved...)
		mu.Unlock()
		if len(before) > 0 {
			cp, err := store.LatestCheckpoint(ctx, project.TenantID, project.ID, job.ID)
			if err != nil || cp == nil || !bytes.Equal(cp.Bytes(), before[len(before)-1].Bytes()) {
				t.Fatalf("committed checkpoint lost after fault: %s %v", checkpointSummary(cp), err)
			}
		}
		current, err := store.GetJob(ctx, project.TenantID, project.ID, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.State == Running {
			if _, err := store.db.Exec(ctx, "UPDATE jobs SET lease_until=now()-interval '1 second' WHERE id=$1", job.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := store.MarkExpired(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Claim(ctx, project.ID, "automatic-replacement"); err != ErrNotFound {
				t.Fatalf("expired attempt automatically reassigned: %v", err)
			}
			current, err = store.GetJob(ctx, project.TenantID, project.ID, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.State != RecoveryRequired {
				t.Fatalf("expired worker state: %s", current.State)
			}
		}
		if current.State == RecoveryRequired {
			cp, err := store.LatestCheckpoint(ctx, project.TenantID, project.ID, job.ID)
			if err != nil && err != ErrNotFound {
				t.Fatal(err)
			}
			evidence := fmt.Sprintf("helper process %d stopped after %s and Wait confirmed exit", child.Process.Pid, fault.mode)
			state, resumeErr := store.ResolveRecovery(ctx, project.TenantID, project.ID, job.ID, "resume", evidence)
			switch workerFaultRecovery(cp) {
			case "resume":
				observed.recovery = "resume"
				if resumeErr != nil || state != Queued {
					t.Fatalf("safe resume: %s %v", state, resumeErr)
				}
				if _, err := store.AdmitOne(ctx); err != nil {
					t.Fatal(err)
				}
			case "complete":
				observed.recovery = "complete"
				if resumeErr != ErrConflict {
					t.Fatalf("finished run resumed: %s %v", state, resumeErr)
				}
				if state, err := store.ResolveRecovery(ctx, project.TenantID, project.ID, job.ID, "complete", evidence); err != nil || state != Succeeded {
					t.Fatalf("finished run not completed: %s %v", state, err)
				}
			default:
				if resumeErr != ErrConflict {
					t.Fatalf("uncertain boundary resumed: %s %v", state, resumeErr)
				}
				want = RecoveryRequired
				observed.recovery = "recovery_required"
			}
		}
		_, done, stderr = start()
	}
	if err := <-done; err != nil {
		t.Fatalf("completion worker: %v %s", err, stderr.String())
	}
	mu.Lock()
	defer mu.Unlock()
	after, err := store.GetJob(ctx, project.TenantID, project.ID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	cp, cpErr := store.LatestCheckpoint(ctx, project.TenantID, project.ID, job.ID)
	if cpErr != nil && cpErr != ErrNotFound {
		t.Fatal(cpErr)
	}
	if after.State != want {
		t.Fatalf("final state: %s, want %s (checkpoint %s)", after.State, want, checkpointSummary(cp))
	}
	for n := 1; n <= 2; n++ {
		if count := modelCalls[n].Load(); count > 1 || (want == Succeeded && count != 1) {
			t.Fatalf("turn %d executed %d times", n, count)
		}
	}
	if want == Succeeded && (after.Result == nil || after.Result.Turns != 2 || after.Result.Final != "finished after process restart") {
		t.Fatalf("two-turn result lost: %+v", after.Result)
	}
	for i, snapshot := range saved {
		if i > 0 && snapshot.Sequence <= saved[i-1].Sequence {
			t.Fatalf("checkpoint sequence regressed: %d after %d", snapshot.Sequence, saved[i-1].Sequence)
		}
	}
	for _, ack := range acknowledged {
		if cp == nil || cp.Sequence < ack.Sequence {
			t.Fatalf("acknowledged checkpoint %d lost: %s", ack.Sequence, checkpointSummary(cp))
		}
	}
	if len(saved) > 0 && (cp == nil || !bytes.Equal(cp.Bytes(), saved[len(saved)-1].Bytes())) {
		t.Fatal("final stored checkpoint differs from accepted checkpoint sequence")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "retained after worker death" {
		t.Fatalf("untracked workspace marker lost: %q %v", data, err)
	}
	return observed
}

func TestPostgresKilledWorkerResumesAtSafeBoundary(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	project, workerToken := testProject(t, store)
	job, _, err := store.Submit(ctx, testSubmission(project, "killed-worker", "two turns"))
	if err != nil {
		t.Fatal(err)
	}
	saved := make(chan struct{}, 1)
	release := make(chan struct{})
	base := (&HTTPServer{Store: store, OperatorToken: "operator-token-abcdefghijklmnopqrstuvwxyz",
		ScalerToken: "scaler-token-abcdefghijklmnopqrstuvwxyz12"}).Handler()
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		block := false
		if strings.HasSuffix(r.URL.Path, "/checkpoint") && r.Method == "POST" {
			raw, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				http.Error(w, readErr.Error(), 500)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
			var request checkpointRequest
			if json.Unmarshal(raw, &request) == nil && request.Checkpoint.Boundary == "turn_complete" {
				block = true
			}
		}
		if !block {
			base.ServeHTTP(w, r)
			return
		}
		record := httptest.NewRecorder()
		base.ServeHTTP(record, r)
		if record.Code == 200 {
			select {
			case saved <- struct{}{}:
			default:
			}
			<-release
		}
		for key, values := range record.Header() {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(record.Code)
		_, _ = w.Write(record.Body.Bytes())
	}))
	defer control.Close()
	defer close(release)
	workspace := t.TempDir()
	marker := filepath.Join(workspace, "workspaces", job.ID, "untracked.txt")
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			if err := os.WriteFile(marker, []byte("retained after worker death"), 0o600); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			fakeTurn(w, "pause", "pause_turn")
			return
		}
		fakeTurn(w, "finished after process restart", "end_turn")
	}))
	defer model.Close()
	if admitted, err := store.AdmitOne(ctx); err != nil || admitted != job.ID {
		t.Fatalf("admit: %s %v", admitted, err)
	}
	binary := realHRN(t)
	child := exec.Command(os.Args[0], "-test.run=^TestWorkerHelperProcess$")
	child.Env = append(os.Environ(),
		"HRN_WORKER_HELPER=1",
		"HRN_WORKER_HRN="+binary,
		"HRN_WORKER_CONTROL="+control.URL,
		"HRN_WORKER_MODEL="+model.URL,
		"HRN_WORKER_PROJECT="+project.ID,
		"HRN_WORKER_TOKEN="+workerToken,
		"HRN_WORKER_ROOT="+workspace)
	var stderr bytes.Buffer
	child.Stderr = &stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	select {
	case <-saved:
	case err := <-done:
		t.Fatalf("worker exited before checkpoint: %v %s", err, stderr.String())
	case <-time.After(30 * time.Second):
		_ = child.Process.Kill()
		<-done
		t.Fatal("worker never saved turn_complete")
	}
	cp, err := store.LatestCheckpoint(ctx, project.TenantID, project.ID, job.ID)
	if err != nil || cp.Boundary != "turn_complete" || cp.Pending != nil || cp.NextTurn != 2 || calls.Load() != 1 {
		_ = child.Process.Kill()
		<-done
		t.Fatalf("checkpoint before kill: %+v %v calls=%d", cp, err, calls.Load())
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("helper unexpectedly exited cleanly")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "retained after worker death" {
		t.Fatalf("untracked file lost after killed worker: %q %v", data, err)
	}
	if _, err := store.db.Exec(ctx, "UPDATE jobs SET lease_until=now()-interval '1 second' WHERE id=$1", job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkExpired(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(ctx, project.ID, "new-worker"); err != ErrNotFound {
		t.Fatalf("killed worker job auto-reassigned: %v", err)
	}
	state, err := store.ResolveRecovery(ctx, project.TenantID, project.ID, job.ID, "resume",
		fmt.Sprintf("helper process %d killed and Wait confirmed exit", child.Process.Pid))
	if err != nil || state != Queued {
		t.Fatalf("operator recovery: %s %v", state, err)
	}
	if _, err := store.AdmitOne(ctx); err != nil {
		t.Fatal(err)
	}
	worker := &Worker{HRNPath: realHRN(t), ControlURL: control.URL, ProjectID: project.ID, Token: workerToken,
		WorkerID: "new-worker", WorkspaceRoot: workspace, ModelURL: model.URL, ModelKey: "fixture"}
	ran, err := worker.RunOnce(ctx)
	if err != nil || !ran {
		t.Fatalf("replacement worker: %v %v", ran, err)
	}
	after, err := store.GetJob(ctx, project.TenantID, project.ID, job.ID)
	if err != nil || after.State != Succeeded || after.Generation != 2 || after.Result == nil ||
		after.Result.Final != "finished after process restart" || after.Result.Turns != 2 || calls.Load() != 2 {
		t.Fatalf("process recovery: %+v %v model calls=%d", after, err, calls.Load())
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "retained after worker death" {
		t.Fatalf("untracked file lost on replacement worker: %q %v", data, err)
	}
}
