package batch

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/tools"
)

func fakeGate(t *testing.T, res tools.ShellResult, err error) {
	t.Helper()
	old := runGateShell
	t.Cleanup(func() { runGateShell = old })
	runGateShell = func(context.Context, string, string, time.Duration, []string) (tools.ShellResult, error) {
		return res, err
	}
}

func gateUTF16(s string, order binary.ByteOrder, bom bool) string {
	var out []byte
	var buf [2]byte
	if bom {
		order.PutUint16(buf[:], 0xfeff)
		out = append(out, buf[:]...)
	}
	for _, u := range utf16.Encode([]rune(s)) {
		order.PutUint16(buf[:], u)
		out = append(out, buf[:]...)
	}
	return string(out)
}

func TestGateDecodesUTF16BeforeKeepingTheOutputTail(t *testing.T) {
	text := strings.Repeat("gate output ", 400) + "failed: café ✓ \U0001f680"
	for _, tc := range []struct {
		name  string
		order binary.ByteOrder
		bom   bool
	}{
		{"little endian BOM", binary.LittleEndian, true},
		{"little endian no BOM", binary.LittleEndian, false},
		{"big endian BOM", binary.BigEndian, true},
		{"big endian no BOM", binary.BigEndian, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			fakeGate(t, tools.ShellResult{Output: gateUTF16(text, tc.order, tc.bom), ExitCode: 1}, nil)
			g := runGate(context.Background(), t.TempDir(), "fake gate", time.Minute)
			if g.Output != tail(text, 4000) {
				t.Fatalf("output tail starts with %.80q, want decoded UTF-8", g.Output)
			}
		})
	}
}

func TestGateInfrastructureFailuresStopRetriesAndKeepTheReason(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		err    error
		want   string
	}{
		{"start error", "", errors.New("process failed to start: permission denied"), "permission denied"},
		{"missing interpreter", "", &exec.Error{Name: "missing-shell", Err: exec.ErrNotFound}, "missing-shell"},
		{"WSL code", gateUTF16("Error code: Wsl/Service/CreateInstance/CreateVm/HCS_E_CONNECTION_TIMEOUT", binary.LittleEndian, true), nil, "HCS_E_CONNECTION_TIMEOUT"},
		{"WSL launcher", gateUTF16("The Windows Subsystem for Linux has no installed distributions.", binary.LittleEndian, true), nil, "The Windows Subsystem for Linux"},
		{"UTF16 WSL BOM", gateUTF16("Error code: HCS_E_CONNECTION_TIMEOUT", binary.LittleEndian, true), nil, "HCS_E_CONNECTION_TIMEOUT"},
		{"UTF16 WSL no BOM", gateUTF16("The Windows Subsystem for Linux failed to start.", binary.LittleEndian, false), nil, "The Windows Subsystem for Linux"},
		{"WSL code before tail", gateUTF16("HCS_E_CONNECTION_TIMEOUT\n"+strings.Repeat("detail\n", 800), binary.LittleEndian, true), nil, "HCS_E_CONNECTION_TIMEOUT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := initRepo(t)
			fakeGate(t, tools.ShellResult{Output: tc.output, ExitCode: 1}, tc.err)
			res, events := run(t, Spec{Dir: dir, Worktrees: "fresh", Backend: "wb-ok", Retry: []string{"wb-ok"},
				Gate: []string{"fake gate", "must not run"}, Tasks: []Task{{ID: "infra", Prompt: "do the thing"}}})
			tr := res.Tasks[0]
			if tr.Status != StatusFail || len(tr.Attempts) != 1 {
				t.Fatalf("status = %s, attempts = %d, want failure without retry", tr.Status, len(tr.Attempts))
			}
			if !strings.Contains(tr.Error, "infrastructure failure") || !strings.Contains(tr.Error, tc.want) {
				t.Errorf("task error = %q, want infrastructure reason %q", tr.Error, tc.want)
			}
			if len(tr.Attempts[0].Gates) != 1 || tr.Commit == "" {
				t.Fatalf("task did not stop its gates and preserve the work: %+v", tr)
			}
			g := tr.Attempts[0].Gates[0]
			if tc.err != nil && g.ExitCode != -1 {
				t.Errorf("start error exit code = %d, want -1", g.ExitCode)
			}
			if !strings.Contains(g.Outcome(), "infrastructure failure") || strings.ContainsRune(g.Output, 0) {
				t.Errorf("gate = %+v", g)
			}
			if eventTexts(events, "retry") != "" || !strings.Contains(eventTexts(events, "gate"), "infrastructure failure") {
				t.Errorf("events = %+v", events)
			}
			saved, err := LoadResult(res.ID)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(saved.Tasks[0].Attempts[0].Gates[0])
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), `"infrastructure_error":`) {
				t.Errorf("saved gate lacks infrastructure reason: %s", raw)
			}
			if saved.Tasks[0].Attempts[0].Gates[0] != g || saved.Tasks[0].Error != tr.Error {
				t.Error("saved record did not preserve the gate and task reason")
			}
		})
	}
}

func TestNormalGateFailuresStillRetryWithDecodedOutput(t *testing.T) {
	for _, output := range []string{"test failed: café", "build-tool: command not found", "test mentions HCS_E_ but no error code"} {
		t.Run(output, func(t *testing.T) {
			dir := initRepo(t)
			fakeGate(t, tools.ShellResult{Output: gateUTF16(output, binary.LittleEndian, true), ExitCode: 127}, nil)
			res, events := run(t, Spec{Dir: dir, Worktrees: "fresh", Backend: "wb-ok", Retry: []string{"wb-ok"},
				Gate: []string{"fake gate"}, Tasks: []Task{{ID: "code", Prompt: "do the thing"}}})
			tr := res.Tasks[0]
			if tr.Status != StatusFail || len(tr.Attempts) != 2 {
				t.Fatalf("task = %+v", tr)
			}
			if tr.Error != "" || eventTexts(events, "retry") == "" || !strings.Contains(tr.Attempts[1].Prompt, output) || strings.ContainsRune(tr.Attempts[1].Prompt, 0) {
				t.Errorf("normal failure did not retry with decoded output: %+v", tr)
			}
		})
	}
}

func TestGateLauncherMentionsAfterTestFailuresStillRetry(t *testing.T) {
	for _, phrase := range []string{"HCS_E_CONNECTION_TIMEOUT", "The Windows Subsystem for Linux"} {
		for _, encoding := range []string{"UTF8", "UTF16"} {
			t.Run(encoding+"/"+phrase, func(t *testing.T) {
				dir := initRepo(t)
				output := "--- FAIL: TestLauncher (0.00s)\n"
				if encoding == "UTF16" {
					output += strings.Repeat("ordinary failing test output\n", 40)
				}
				output += "    gate_test.go:1: unexpected " + phrase + "\nFAIL\n"
				captured := output
				if encoding == "UTF16" {
					captured = gateUTF16(output, binary.LittleEndian, true)
				}
				fakeGate(t, tools.ShellResult{Output: captured, ExitCode: 1}, nil)
				res, events := run(t, Spec{Dir: dir, Worktrees: "fresh", Backend: "wb-ok", Retry: []string{"wb-ok"},
					Gate: []string{"fake gate"}, Tasks: []Task{{ID: "code", Prompt: "do the thing"}}})
				tr := res.Tasks[0]
				if tr.Status != StatusFail || len(tr.Attempts) != 2 {
					t.Fatalf("status = %s, attempts = %d, want failure with retry", tr.Status, len(tr.Attempts))
				}
				if tr.Error != "" || eventTexts(events, "retry") == "" || !strings.Contains(tr.Attempts[1].Prompt, output) {
					t.Errorf("ordinary test failure did not retry with its output: %+v", tr)
				}
				for _, att := range tr.Attempts {
					if g := att.Gates[0]; g.InfrastructureError != "" {
						t.Errorf("ordinary test failure classified as infrastructure: %+v", g)
					}
				}
			})
		}
	}
}

func TestGateKeepsUTF8AndSuccessfulLauncherMentionsUnchanged(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	output := "The Windows Subsystem for Linux: HCS_E_CONNECTION_TIMEOUT test passed ✓"
	fakeGate(t, tools.ShellResult{Output: output}, nil)
	g := runGate(context.Background(), t.TempDir(), "fake gate", time.Minute)
	if g.Output != output || g.Outcome() != "exited 0" {
		t.Errorf("gate = %+v", g)
	}
	raw, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "infrastructure_error") {
		t.Errorf("successful gate has an infrastructure field: %s", raw)
	}
}

func TestGateOutputDetectionHandlesShortAndIncompleteText(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, tc := range []struct{ input, want string }{
		{"", ""},
		{"x", "x"},
		{"x\x00", "x\x00"},           // too short for detection without a BOM
		{"abc\x00def", "abc\x00def"}, // isolated NUL is not UTF-16
		{"\xff\xfe", ""},
		{"\xff\xfex\x00y", "x\ufffd"},  // incomplete final code unit
		{"\xff\xfe\x00\xd8", "\ufffd"}, // unpaired surrogate
	} {
		fakeGate(t, tools.ShellResult{Output: tc.input}, nil)
		g := runGate(context.Background(), t.TempDir(), "fake gate", time.Minute)
		if g.Output != tc.want {
			t.Errorf("output for %q = %q, want %q", tc.input, g.Output, tc.want)
		}
	}
}

// A gate may run for hours and writes nothing of its own: while it runs, the
// batch's record is saved again, so a reader does not take it for stopped.
func TestABatchWhoseGateRunsPastTheQuietHourIsNotShownAsStopped(t *testing.T) {
	dir := initRepo(t)
	old := gateBeat
	gateBeat = 20 * time.Millisecond
	t.Cleanup(func() { gateBeat = old })
	batches := filepath.Join(os.Getenv("HRN_HOME"), "batches")
	var during Result
	var alive bool
	old2 := runGateShell
	t.Cleanup(func() { runGateShell = old2 })
	runGateShell = func(context.Context, string, string, time.Duration, []string) (tools.ShellResult, error) {
		entries, err := os.ReadDir(batches)
		if err != nil || len(entries) != 1 {
			t.Errorf("batches while the gate runs: %v %v", entries, err)
			return tools.ShellResult{}, nil
		}
		path := filepath.Join(batches, entries[0].Name())
		quiet := time.Now().Add(-2 * time.Hour)
		if err := os.Chtimes(path, quiet, quiet); err != nil {
			t.Error(err)
		}
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end) && !alive; time.Sleep(10 * time.Millisecond) {
			if info, err := os.Stat(path); err == nil {
				alive = runlog.Active(info.ModTime())
			}
		}
		if data, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(data, &during)
		}
		return tools.ShellResult{}, nil
	}
	run(t, Spec{Dir: dir, Backend: "wb-ok", Gate: []string{"long build"}, Tasks: []Task{{ID: "slow", Prompt: "a"}}})
	if !alive {
		t.Fatal("the record was not saved again while the gate ran past the quiet hour")
	}
	if len(during.Tasks) != 1 || during.Tasks[0].Status != StatusRunning || during.Duration != 0 {
		t.Errorf("record while the gate ran = %+v", during)
	}
	// A late beat would save the task as running again, without its gate.
	time.Sleep(5 * gateBeat)
	entries, err := os.ReadDir(batches)
	if err != nil || len(entries) != 1 {
		t.Fatalf("batches after the run: %v %v", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(batches, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var after Result
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatal(err)
	}
	if len(after.Tasks) != 1 || after.Tasks[0].Status != StatusPass || len(after.Tasks[0].Attempts) != 1 || len(after.Tasks[0].Attempts[0].Gates) != 1 {
		t.Errorf("record after the run = %+v", after)
	}
}

func TestBeatingNeverBeatsAfterStopReturns(t *testing.T) {
	var started, finished atomic.Int32
	stop := beating(time.Millisecond, func() {
		started.Add(1)
		time.Sleep(5 * time.Millisecond)
		finished.Add(1)
	})
	for end := time.Now().Add(5 * time.Second); started.Load() < 2 && time.Now().Before(end); time.Sleep(time.Millisecond) {
	}
	stop()
	n := started.Load()
	if n < 2 {
		t.Fatalf("beats before stop = %d", n)
	}
	if f := finished.Load(); f != n {
		t.Errorf("stop returned with a beat still running: %d started, %d finished", n, f)
	}
	time.Sleep(20 * time.Millisecond)
	if got := started.Load(); got != n {
		t.Errorf("beats after stop = %d, want %d", got, n)
	}
}
