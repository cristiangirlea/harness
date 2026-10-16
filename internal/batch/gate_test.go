package batch

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

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
