package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cristiangirlea/harness/internal/pluginproto"
	"github.com/cristiangirlea/harness/internal/tools"
)

var testHost = pluginproto.Program{Name: "hrn", Version: "0.2.0"}

func TestStartReportsTheNegotiatedPluginAndItsSHA256(t *testing.T) {
	session, exe := startFake(t, "echo")
	info := session.Info()
	if info.Name != "echo" || info.Version != "0.1.0" || info.Protocol != pluginproto.V1 {
		t.Fatalf("Info() = %+v", info)
	}
	names := make([]string, 0, len(info.Capabilities))
	for _, capability := range info.Capabilities {
		names = append(names, capability.Name)
	}
	if !slices.Equal(names, []string{"echo", "wait", "env", "exit", "skip", "late", "latebare", "spawn", "complain", "wrongid", "alien"}) {
		t.Errorf("capabilities = %v", names)
	}
	assertSamePath(t, info.Path, exe)
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("%x", sha256.Sum256(data)); info.SHA256 != want {
		t.Errorf("SHA256 = %s, want %s", info.SHA256, want)
	}
}

func TestCallDeliversTheEventAndReturnsEchoesWithSuccessiveIDs(t *testing.T) {
	session, _ := startFake(t, "echo")
	input := json.RawMessage(`{"message":"hi","number":5}`)
	var events []pluginproto.Event
	result, err := session.Call(t.Context(), "echo", input, 2*time.Second, func(event pluginproto.Event) {
		events = append(events, event)
	})
	if err != nil || result == nil || result.ID != "c1" || result.Status != pluginproto.StatusOK || !bytes.Equal(result.Output, input) {
		t.Fatalf("first Call() = %+v, %v", result, err)
	}
	if len(events) != 1 || events[0].ID != "c1" || events[0].Kind != pluginproto.KindProgress || events[0].Seq != 2 {
		t.Errorf("events = %+v", events)
	}
	result, err = session.Call(t.Context(), "echo", nil, 2*time.Second, nil)
	if err != nil || result == nil || result.ID != "c2" || string(result.Output) != `{}` {
		t.Fatalf("second Call() = %+v, %v", result, err)
	}
}

func TestCallReturnsTheCanceledResultAndTheContextError(t *testing.T) {
	session, _ := startFake(t, "echo")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result, err := session.Call(ctx, "wait", nil, 2*time.Second, func(pluginproto.Event) { cancel() })
	if !errors.Is(err, context.Canceled) || result == nil || result.Status != pluginproto.StatusCanceled {
		t.Fatalf("Call() = %+v, %v", result, err)
	}
}

func TestCallReturnsTheCanceledResultAndTheTimeoutError(t *testing.T) {
	session, exe := startFake(t, "echo")
	result, err := session.Call(t.Context(), "wait", nil, 300*time.Millisecond, nil)
	if err == nil || !strings.Contains(err.Error(), "call c1 timed out after 300ms") || result == nil || result.Status != pluginproto.StatusCanceled {
		t.Fatalf("Call() = %+v, %v", result, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "%!") {
		t.Errorf("timeout error %q must wrap the deadline and nothing else", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	frames := receivedFrames(t, exe)
	if len(frames) != 3 {
		t.Fatalf("received %d frames, want hello, call and cancel", len(frames))
	}
	call, ok := frames[1].(*pluginproto.Call)
	if !ok || call.TimeoutMS != 300 {
		t.Errorf("received call = %+v", frames[1])
	}
	if cancel, ok := frames[2].(*pluginproto.Cancel); !ok || cancel.ID != "c1" {
		t.Errorf("received cancel = %+v", frames[2])
	}
}

func TestCallSkipsTheBadRequestForACancelThatCrossedAResult(t *testing.T) {
	session, _ := startFake(t, "echo")
	assertCrossingCancel(t, session, "late")
	result, err := session.Call(t.Context(), "echo", nil, 2*time.Second, nil)
	if err != nil || result == nil || result.ID != "c2" || result.Status != pluginproto.StatusOK {
		t.Fatalf("Call(echo) = %+v, %v", result, err)
	}
}

func TestCallFailsOnAnErrorWithoutAnIDAfterACancelCrossedAResult(t *testing.T) {
	session, _ := startFake(t, "echo")
	assertCrossingCancel(t, session, "latebare")
	if result, err := session.Call(t.Context(), "echo", nil, 2*time.Second, nil); err == nil || result != nil || !strings.Contains(err.Error(), `received error code "bad_request"`) {
		t.Fatalf("Call(echo) = %+v, %v", result, err)
	}
	if _, err := session.Call(t.Context(), "echo", nil, time.Second, nil); err == nil {
		t.Error("Call() accepted a call on a broken session")
	}
}

func assertCrossingCancel(t *testing.T, session *Session, capability string) {
	t.Helper()
	result, err := session.Call(t.Context(), capability, nil, 300*time.Millisecond, nil)
	if err == nil || !strings.Contains(err.Error(), "call c1 timed out after 300ms") || result == nil || result.ID != "c1" || result.Status != pluginproto.StatusOK {
		t.Fatalf("Call(%s) = %+v, %v", capability, result, err)
	}
}

func TestCallKillsAPluginThatIgnoresCancellation(t *testing.T) {
	session, _ := startFake(t, "stubborn")
	started := time.Now()
	result, err := session.Call(t.Context(), "wait", nil, 300*time.Millisecond, nil)
	if err == nil || !strings.Contains(err.Error(), "timed out") || result != nil {
		t.Fatalf("Call() = %+v, %v", result, err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("Call() took %s", elapsed)
	}
	if _, err := session.Call(t.Context(), "wait", nil, time.Second, nil); err == nil {
		t.Error("Call() accepted a call on a broken session")
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if session.cmd.ProcessState == nil || session.cmd.ProcessState.Success() {
		t.Errorf("process state after Close() = %v", session.cmd.ProcessState)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("second Close() = %v", err)
	}
}

func TestStartAppliesTheEnvironmentPolicyAndGuard(t *testing.T) {
	filtered := []string{"HRN_OPERATOR_TOKEN", "HRN_DATABASE_URL", "ANTHROPIC_API_KEY", "GIT_DIR", "HRN_WATCH_ADDR", "HRN_TLS_KEY_FILE", "hrn_MIXED_CASE"}
	for _, name := range filtered {
		t.Setenv(name, "x")
	}
	t.Setenv("GH_TOKEN", "real-secret")
	session, _ := startFake(t, "echo")
	filtered = append(filtered, "HRN_HOME")
	input, err := json.Marshal(map[string][]string{"names": append(slices.Clone(filtered), tools.GuardedVar, "GH_TOKEN")})
	if err != nil {
		t.Fatal(err)
	}
	result, err := session.Call(t.Context(), "env", input, 2*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]envEntry
	if err := json.Unmarshal(result.Output, &env); err != nil {
		t.Fatal(err)
	}
	for _, name := range filtered {
		if entry, ok := env[name]; !ok || entry.Set {
			t.Errorf("expected %s to be reported unset", name)
		}
	}
	if marker := env[tools.GuardedVar]; !marker.Set || marker.Value != "1" {
		t.Errorf("expected the child guard marker to be set to 1")
	}
	if token, ok := env["GH_TOKEN"]; !ok || token.Value == "real-secret" {
		t.Error("expected the child to report a guarded GH_TOKEN")
	}
}

func TestCallBreaksTheSessionWhenThePluginExitsWithoutAResult(t *testing.T) {
	assertCallBreaksSession(t, "exit", "exited before the result")
}

func TestCallBreaksTheSessionWhenThePluginSkipsASequenceNumber(t *testing.T) {
	assertCallBreaksSession(t, "skip", "sequence")
}

func TestCallBreaksTheSessionWhenThePluginSendsAnErrorInsteadOfAResult(t *testing.T) {
	assertCallBreaksSession(t, "complain", `received error code "internal"`)
}

func TestCallBreaksTheSessionWhenTheResultHasAnotherCallID(t *testing.T) {
	assertCallBreaksSession(t, "wrongid", `unexpected call ID "zz"`)
}

func TestCallBreaksTheSessionWhenThePluginSendsAnUnknownFrameType(t *testing.T) {
	assertCallBreaksSession(t, "alien", `unknown plugin frame "alien"`)
}

func assertCallBreaksSession(t *testing.T, capability, diagnostic string) {
	t.Helper()
	session, _ := startFake(t, "echo")
	result, err := session.Call(t.Context(), capability, nil, 2*time.Second, nil)
	if err == nil || !strings.Contains(err.Error(), diagnostic) || result != nil {
		t.Fatalf("Call(%q) = %+v, %v", capability, result, err)
	}
	if _, err := session.Call(t.Context(), "echo", nil, time.Second, nil); err == nil {
		t.Error("Call() accepted a call on a broken session")
	}
}

func TestCallWritesNothingForInvalidOrConcurrentCalls(t *testing.T) {
	session, exe := startFake(t, "echo")
	if _, err := session.Call(t.Context(), "unknown", nil, time.Second, nil); err == nil {
		t.Error("Call() accepted an unknown capability")
	}
	if _, err := session.Call(t.Context(), "echo", json.RawMessage(`[]`), time.Second, nil); err == nil {
		t.Error("Call() accepted an array input")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	progress, finished := make(chan struct{}), make(chan error, 1)
	go func() {
		result, err := session.Call(ctx, "wait", nil, 5*time.Second, func(pluginproto.Event) { close(progress) })
		if !errors.Is(err, context.Canceled) || result == nil || result.Status != pluginproto.StatusCanceled {
			finished <- fmt.Errorf("waiting Call() = %+v, %v", result, err)
			return
		}
		finished <- nil
	}()
	select {
	case <-progress:
	case <-time.After(2 * time.Second):
		t.Fatal("waiting call did not report progress")
	}
	if _, err := session.Call(t.Context(), "echo", nil, time.Second, nil); err == nil {
		t.Error("Call() accepted a concurrent call")
	}
	cancel()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	frames := receivedFrames(t, exe)
	if len(frames) != 3 {
		t.Fatalf("received %d frames, want hello, wait call and cancel", len(frames))
	}
	if _, ok := frames[0].(*pluginproto.Hello); !ok {
		t.Errorf("first frame = %+v", frames[0])
	}
	if call, ok := frames[1].(*pluginproto.Call); !ok || call.Capability != "wait" || call.ID != "c1" {
		t.Errorf("second frame = %+v", frames[1])
	}
	if _, ok := frames[2].(*pluginproto.Cancel); !ok {
		t.Errorf("third frame = %+v", frames[2])
	}
}

func TestCallWithAnAlreadyCanceledContextWritesNothing(t *testing.T) {
	session, exe := startFake(t, "echo")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if result, err := session.Call(ctx, "echo", nil, time.Second, nil); result != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Call() = %+v, %v", result, err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	frames := receivedFrames(t, exe)
	if len(frames) != 1 {
		t.Fatalf("received %d frames, want only hello", len(frames))
	}
	if _, ok := frames[0].(*pluginproto.Hello); !ok {
		t.Errorf("first frame = %+v", frames[0])
	}
}

func TestCallWithAnAlreadyCanceledContextLeavesTheFirstIDAvailable(t *testing.T) {
	session, _ := startFake(t, "echo")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := session.Call(ctx, "echo", nil, time.Second, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Call() error = %v", err)
	}
	if result, err := session.Call(t.Context(), "echo", nil, 2*time.Second, nil); err != nil || result == nil || result.ID != "c1" || result.Status != pluginproto.StatusOK {
		t.Fatalf("next Call() = %+v, %v", result, err)
	}
}

func TestCallTimesOutWhenThePluginStopsReadingStdin(t *testing.T) {
	session, _ := startFake(t, "deaf")
	input := json.RawMessage(`{"text":"` + strings.Repeat("x", 3<<20) + `"}`)
	started := time.Now()
	if result, err := session.Call(t.Context(), "echo", input, 300*time.Millisecond, nil); result != nil || err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("Call() = %+v, %v", result, err)
	}
	if elapsed := time.Since(started); elapsed > 6*time.Second {
		t.Errorf("Call() took %s", elapsed)
	}
	started = time.Now()
	if err := session.Close(); err != nil {
		t.Error(err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("Close() took %s", elapsed)
	}
}

func TestStartRefusesAPluginThatStaysSilent(t *testing.T) {
	assertRefused(t, "silent", "timed out")
}

func TestStartRefusesAPluginThatSendsInvalidJSON(t *testing.T) {
	assertRefused(t, "garbage", "expected a JSON object")
}

func TestStartRefusesAPluginThatAnswersWithAnotherName(t *testing.T) {
	assertRefused(t, "othername", "someone-else")
}

func TestStartRefusesAPluginThatSendsAnOverlongLine(t *testing.T) {
	assertRefused(t, "longline", "exceeds")
}

func TestStartReportsTheUnsupportedProtocolAndOfferedVersions(t *testing.T) {
	assertRefused(t, "oldproto", `"unsupported_protocol"`, `"v2"`)
}

func TestStartIncludesStderrWhenThePluginExitsBeforeReady(t *testing.T) {
	assertRefused(t, "noisy", "boom on stderr")
}

func TestStartRefusesAReadyFrameWithALateSequenceNumber(t *testing.T) {
	assertRefused(t, "lateseq", "ready sequence is 2, want 1")
}

func TestStartRefusesAReadyFrameThatSelectsAnUnofferedProtocol(t *testing.T) {
	assertRefused(t, "v9ready", `ready selected protocol "v9"`)
}

func TestStartSanitizesStderrInTheErrorAndKeepsTheRawCopy(t *testing.T) {
	home := pluginHome(t)
	installFake(t, home, "escape")
	found, err := Find("escape")
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	session, err := Start(t.Context(), found, Options{Host: testHost, Stderr: &stderr, HandshakeTimeout: 2 * time.Second})
	if session != nil {
		_ = session.Close()
		t.Fatal("Start() accepted escape")
	}
	if err == nil || !strings.Contains(err.Error(), "boom") || strings.ContainsAny(err.Error(), "\x1b\x07\r") || !utf8.ValidString(err.Error()) {
		t.Errorf("Start() error = %q", err)
	}
	if stderr.String() != escapedStderr {
		t.Errorf("stderr copy = %q, want %q", stderr.String(), escapedStderr)
	}
}

func assertRefused(t *testing.T, name string, diagnostics ...string) {
	t.Helper()
	home := pluginHome(t)
	exe := installFake(t, home, name)
	found, err := Find(name)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	session, err := Start(t.Context(), found, Options{Host: testHost, Stderr: &stderr, HandshakeTimeout: 2 * time.Second})
	if session != nil {
		_ = session.Close()
		t.Fatal("Start() accepted a plugin that should be refused")
	}
	if err == nil || !strings.HasPrefix(err.Error(), "plugin "+name+": ") {
		t.Fatalf("Start() error = %v", err)
	}
	for _, diagnostic := range diagnostics {
		if !strings.Contains(err.Error(), diagnostic) {
			t.Errorf("Start() error = %q, want %q", err, diagnostic)
		}
	}
	if name == "noisy" && !strings.Contains(stderr.String(), "boom on stderr") {
		t.Errorf("stderr copy = %q", stderr.String())
	}
	frames := receivedFrames(t, exe)
	if len(frames) != 1 {
		t.Fatalf("received %d frames, want only hello", len(frames))
	}
	if _, ok := frames[0].(*pluginproto.Hello); !ok {
		t.Errorf("first frame = %+v", frames[0])
	}
}

func TestCloseLetsAHealthyPluginExitSuccessfully(t *testing.T) {
	session, _ := startFake(t, "echo")
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if session.cmd.ProcessState == nil || !session.cmd.ProcessState.Success() {
		t.Errorf("process state after Close() = %v", session.cmd.ProcessState)
	}
	if _, err := session.Call(t.Context(), "echo", nil, time.Second, nil); err == nil {
		t.Error("Call() accepted a call after Close()")
	}
	if _, err := os.Stat(session.guardDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("guard directory after Close(): %v", err)
	}
}

func TestCloseReturnsPromptlyWhenAHealthyPluginIgnoresEndOfInput(t *testing.T) {
	session, _ := startFake(t, "lingering")
	started := time.Now()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 6*time.Second {
		t.Errorf("Close() took %s", elapsed)
	}
}

func TestCloseReturnsTheSameExitErrorWithStderrOnRepeatedCalls(t *testing.T) {
	session, _ := launchFake(t, "sulky")
	err := session.Close()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 4 || !strings.Contains(err.Error(), "leaving badly") {
		t.Fatalf("Close() = %v", err)
	}
	if again := session.Close(); again != err {
		t.Errorf("second Close() = %v, want the same error as the first Close()", again)
	}
}

func TestCloseStopsAPluginDescendantFromWritingItsHeartbeat(t *testing.T) {
	session, _ := startFake(t, "echo")
	file := filepath.Join(t.TempDir(), "heartbeat")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(map[string]string{"file": file})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := session.Call(t.Context(), "spawn", input, 2*time.Second, nil); err != nil || result == nil || result.Status != pluginproto.StatusOK {
		t.Fatalf("Call(spawn) = %+v, %v", result, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for heartbeatSize(t, file) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("descendant did not write a heartbeat")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	size := heartbeatSize(t, file)
	time.Sleep(500 * time.Millisecond)
	if got := heartbeatSize(t, file); got != size {
		t.Errorf("heartbeat size grew from %d to %d after Close()", size, got)
	}
}

func heartbeatSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func TestCloseFromAnotherGoroutineUnblocksACallWithoutADeadline(t *testing.T) {
	session, _ := startFake(t, "echo")
	progress, finished := make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := session.Call(context.Background(), "wait", nil, 0, func(pluginproto.Event) { close(progress) })
		finished <- err
	}()
	select {
	case <-progress:
	case <-time.After(2 * time.Second):
		t.Fatal("waiting call did not report progress")
	}
	closed := make(chan error, 1)
	go func() { closed <- session.Close() }()
	select {
	case err := <-finished:
		if err == nil {
			t.Error("Call() succeeded while its session was closing")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close() did not unblock Call()")
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

func TestStderrTailKeepsTheLastEightKiBAndCopiesTheWholeStream(t *testing.T) {
	var mirror bytes.Buffer
	tail := &stderrTail{mirror: &mirror}
	first, second := strings.Repeat("a", 9<<10), "\nlast line\n"
	for _, part := range []string{first, second} {
		if n, err := tail.Write([]byte(part)); n != len(part) || err != nil {
			t.Fatalf("Write() = %d, %v", n, err)
		}
	}
	if got := tail.String(); len(got) != 8<<10 || !strings.HasSuffix(got, second) {
		t.Errorf("tail has %d bytes, ending in %q expected", len(got), second)
	}
	if mirror.String() != first+second {
		t.Errorf("mirror received %d bytes, want %d", mirror.Len(), len(first)+len(second))
	}
}

func startFake(t *testing.T, name string) (*Session, string) {
	t.Helper()
	session, exe := launchFake(t, name)
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	return session, exe
}

func launchFake(t *testing.T, name string) (*Session, string) {
	t.Helper()
	home := pluginHome(t)
	exe := installFake(t, home, name)
	found, err := Find(name)
	if err != nil {
		t.Fatal(err)
	}
	session, err := Start(t.Context(), found, Options{Host: testHost, HandshakeTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return session, exe
}
