package batch

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"
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
			g := runGate(context.Background(), t.TempDir(), "fake gate", time.Minute, nil)
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
	g := runGate(context.Background(), t.TempDir(), "fake gate", time.Minute, nil)
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
		g := runGate(context.Background(), t.TempDir(), "fake gate", time.Minute, nil)
		if g.Output != tc.want {
			t.Errorf("output for %q = %q, want %q", tc.input, g.Output, tc.want)
		}
	}
}

func TestGateDecodesLongUTF16AroundTheTruncationMarker(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	head := gateUTF16(strings.Repeat("early line\n", 900), binary.LittleEndian, true)
	text := strings.Repeat("late output café ✓\n", 260) + "FAIL \U0001f680"
	// A five-digit count makes the ASCII marker an odd number of bytes.
	captured := fmt.Sprintf("%s\n\n[... %d bytes truncated ...]\n\n%s", head, 12344, gateUTF16(text, binary.LittleEndian, false))
	fakeGate(t, tools.ShellResult{Output: captured, ExitCode: 1}, nil)
	g := runGate(context.Background(), t.TempDir(), "fake gate", time.Minute, nil)
	if g.Output != tail(text, 4000) {
		t.Fatalf("output tail starts with %.80q, want decoded UTF-8", g.Output)
	}
}

func TestGateKeepsTheByteOrderAfterTheTruncationMarker(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	head := gateUTF16(strings.Repeat("early line\n", 900), binary.LittleEndian, true)
	text := "✓ ok\n" + strings.Repeat("Ошибка ✓\n", 600) + "FAIL"
	captured := fmt.Sprintf("%s\n\n[... %d bytes truncated ...]\n\n%s", head, 1234, gateUTF16(text, binary.LittleEndian, false))
	fakeGate(t, tools.ShellResult{Output: captured, ExitCode: 1}, nil)
	g := runGate(context.Background(), t.TempDir(), "fake gate", time.Minute, nil)
	if g.Output != tail(text, 4000) {
		t.Fatalf("output tail starts with %.80q, want decoded UTF-8", g.Output)
	}
}

func TestGateKeepsAUTF8TailThatStartsInsideACharacter(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	head := gateUTF16(strings.Repeat("early line\n", 900), binary.LittleEndian, true)
	text := " ok\n" + strings.Repeat("late output café ✓\n", 260) + "FAIL"
	// The cut leaves the last two bytes of a UTF-8 ✓.
	captured := fmt.Sprintf("%s\n\n[... %d bytes truncated ...]\n\n\x9c\x93%s", head, 1234, text)
	fakeGate(t, tools.ShellResult{Output: captured, ExitCode: 1}, nil)
	g := runGate(context.Background(), t.TempDir(), "fake gate", time.Minute, nil)
	if g.Output != tail(text, 4000) {
		t.Fatalf("output tail starts with %.80q, want the UTF-8 kept", g.Output)
	}
}

func TestGateDecodesEachEncodingAsIntended(t *testing.T) {
	le := func(s string, bom bool) string { return gateUTF16(s, binary.LittleEndian, bom) }
	cut := func(stream string, keepHead, from int) string {
		return fmt.Sprintf("%s\n\n[... %d bytes truncated ...]\n\n%s", stream[:keepHead], from-keepHead, stream[from:])
	}
	type gateCase struct{ name, captured, want string }
	var cases []gateCase
	for _, text := range []string{
		"error\nОшибка\nmore\n", "error\n测试\nmore\n", "error\nनमस्ते\nmore\n", "error\n上传失败\nmore\n",
		"error\nОшибка", "error\n测试", "error\nनमस्ते", "error\nЊива\nmore\n", "error\n\nmore\n",
		"\x1b[31mFAIL\x1b[0m: Ошибка ✓\n", "error\r\nОшибка\r\nmore\r\n",
	} {
		cases = append(cases, gateCase{fmt.Sprintf("UTF-16LE BOM %q", text), le(text, true), strings.TrimSpace(text)})
	}
	for _, text := range []string{"ok ✓\n--- FAIL: тест\nОшибка", "✓ passed\n测试", "\x1b[32m✓\x1b[0m ok\r\nनमस्ते\r\n"} {
		cases = append(cases, gateCase{fmt.Sprintf("UTF-8 %q", text), text, strings.TrimSpace(text)})
	}
	early := le(strings.Repeat("early line\n", 900), true)
	late := strings.Repeat("late Ошибка ✓\n", 300) + "FAIL"
	short := le("Ошибка ✓\n", true)
	cases = append(cases,
		gateCase{"UTF-16LE launcher line then UTF-8",
			le("wsl: starting\r\n", false) + "--- FAIL: café ✓\n", "wsl: starting\r\n--- FAIL: café ✓"},
		gateCase{"UTF-16LE with a BOM then UTF-8 of odd length",
			le("wsl: starting\n", true) + "--- FAIL: cafés\n", "wsl: starting\n--- FAIL: cafés"},
		gateCase{"UTF-16LE CJK then UTF-8",
			le("error\n上传失败\n", true) + "ok ✓\n", "error\n上传失败\nok ✓"},
		gateCase{"UTF-16LE then UTF-8 whose newline is at an even offset",
			le("wsl: starting\n", false) + "ab\ncde ✓\nmore\n", "wsl: starting\nab\ncde ✓\nmore"},
		gateCase{"UTF-16 head and tail cut on a code-unit boundary",
			cut(early+le("x"+late, false), 6000, len(early)+2), tail(late, 4000)},
		gateCase{"UTF-16 tail cut inside a code unit",
			cut(early+le("x"+late, false), 6000, len(early)+1), tail(late, 4000)},
		gateCase{"UTF-16 head cut inside a character",
			cut(short+le("tail ✓\n", false), 7, len(short)+1), fmt.Sprintf("Ош\n\n[... %d bytes truncated ...]\n\nail ✓", len(short)+1-7)},
		gateCase{"UTF-16 tail cut inside a surrogate pair",
			cut(short+le("x\U0001f680 done ✓\n", false), 7, len(short)+4), fmt.Sprintf("Ош\n\n[... %d bytes truncated ...]\n\n done ✓", len(short)+4-7)},
		gateCase{"UTF-8 head and tail cut inside characters",
			cut("ok ✓ done\nlast ✓ line\n", 4, len("ok ✓ done\nlast ✓")-1), "ok \n\n[... 15 bytes truncated ...]\n\n line"},
	)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			fakeGate(t, tools.ShellResult{Output: tc.captured, ExitCode: 1}, nil)
			g := runGate(context.Background(), t.TempDir(), "fake gate", time.Minute, nil)
			if g.Output != tc.want {
				t.Fatalf("output = %.120q, want %.120q", g.Output, tc.want)
			}
			if main := mainDecodeGateOutput(tc.captured); !gateTruncation.MatchString(tc.captured) && gateBadRunes(main) == 0 && g.Output != tail(main, 4000) {
				t.Fatalf("output = %q, main's decoder gives %q", g.Output, tail(main, 4000))
			}
		})
	}
}

func TestGateSplitsMixedOutputThatMainReadsAsUTF16(t *testing.T) {
	le := func(s string, bom bool) string { return gateUTF16(s, binary.LittleEndian, bom) }
	// Even-length ASCII after the UTF-16 line reads as valid CJK when decoded as UTF-16.
	for _, tc := range []struct{ name, captured, want string }{
		{"UTF-16LE line of 16+ characters then even-length ASCII",
			le("wsl: Unknown key 'x' in /etc/wsl.conf\r\n", false) + "--- FAIL: x\n", "wsl: Unknown key 'x' in /etc/wsl.conf\r\n--- FAIL: x"},
		{"UTF-16LE with a BOM then even-length ASCII",
			le("wsl: starting\n", true) + "--- FAIL: x\n", "wsl: starting\n--- FAIL: x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			fakeGate(t, tools.ShellResult{Output: tc.captured, ExitCode: 1}, nil)
			g := runGate(context.Background(), t.TempDir(), "fake gate", time.Minute, nil)
			if g.Output != tc.want {
				t.Fatalf("output = %.120q, want %.120q", g.Output, tc.want)
			}
		})
	}
}

func TestGateDecoderMatchesMainOnTextInOneEncoding(t *testing.T) {
	alphabets := map[string][]rune{
		"ascii":      []rune("abcXYZ019 -:/.'()"),
		"cyrillic":   gateRunes(0x0410, 0x044f),
		"cjk":        append(gateRunes(0x4e00, 0x4e3f), '上', '中', '测', '试'),
		"japanese":   []rune("上の本あいうアイウ"),
		"korean":     []rune("한국어가나다"),
		"gurmukhi":   gateRunes(0x0a00, 0x0a7f),
		"devanagari": gateRunes(0x0900, 0x097f),
		"emoji":      []rune("✓✗\U0001f680\U0001f600\U0001f525"),
	}
	encodings := map[string]func(string) string{
		"utf8":  func(s string) string { return s },
		"le":    func(s string) string { return gateUTF16(s, binary.LittleEndian, false) },
		"leBOM": func(s string) string { return gateUTF16(s, binary.LittleEndian, true) },
		"be":    func(s string) string { return gateUTF16(s, binary.BigEndian, false) },
		"beBOM": func(s string) string { return gateUTF16(s, binary.BigEndian, true) },
	}
	rng := rand.New(rand.NewPCG(1, 2))
	ascii := alphabets["ascii"]
	for name, alphabet := range alphabets {
		for range 3000 {
			var b strings.Builder
			lines := 1 + rng.IntN(4)
			for l := range lines {
				// Even lines mix in ASCII, as test output does; odd lines have none.
				for range rng.IntN(9) {
					if l%2 == 0 && rng.IntN(3) == 0 {
						b.WriteRune(ascii[rng.IntN(len(ascii))])
					} else {
						b.WriteRune(alphabet[rng.IntN(len(alphabet))])
					}
				}
				if l < lines-1 || rng.IntN(2) == 0 {
					b.WriteString([]string{"\n", "\r\n"}[rng.IntN(2)])
				}
			}
			for enc, encode := range encodings {
				captured := encode(b.String())
				if got, want := decodeGateOutputText(captured), mainDecodeGateOutput(captured); got != want {
					t.Fatalf("%s/%s %q: decoded %q, main gives %q", name, enc, b.String(), got, want)
				}
			}
		}
	}
}

func TestGateSplitsAUTF16LineFromTheUTF8AfterIt(t *testing.T) {
	// A tail like "ok" is also one valid UTF-16 character in one script, so it stays as main reads it.
	tails := []string{
		"--- FAIL: x\n", "--- FAIL: xy\n", "ok\n", "ok\r\n", "FAIL\n", "PASS ok\n", "café ✓\n", "café ✓ ok\n",
		"Ошибка\n", "Ошибка: тест\n", "测试 失败\n", "上传失败\n", "done.", "\x1b[31mFAIL\x1b[0m\r\n", "a\nb\n\n",
	}
	line := "wsl: Unknown key 'automount.x' in /etc/wsl.conf"
	for n := 1; n <= len(line); n++ {
		for _, nl := range []string{"\n", "\r\n"} {
			for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
				for _, bom := range []bool{false, true} {
					for _, rest := range tails {
						captured := gateUTF16(line[:n]+nl, order, bom) + rest
						if got := decodeGateOutputText(captured); got != line[:n]+nl+rest {
							t.Errorf("%v BOM %v %q then %q: decoded %q", order, bom, line[:n], rest, got)
						}
					}
				}
			}
		}
	}
}

func gateRunes(lo, hi rune) []rune {
	var rs []rune
	for r := lo; r <= hi; r++ {
		if unicode.IsGraphic(r) {
			rs = append(rs, r)
		}
	}
	return rs
}

func decodeGateOutputText(s string) string {
	out, _ := decodeGateOutput(s)
	return out
}

// mainDecodeGateOutput is the decoder before mixed output was handled, kept as the reference for clean results.
func mainDecodeGateOutput(s string) string {
	var order binary.ByteOrder
	switch {
	case strings.HasPrefix(s, "\xff\xfe"):
		order, s = binary.LittleEndian, s[2:]
	case strings.HasPrefix(s, "\xfe\xff"):
		order, s = binary.BigEndian, s[2:]
	default:
		n := min(len(s)/2, 16)
		if n < 2 {
			return s
		}
		le, be := true, true
		for i := 0; i < n*2; i += 2 {
			le = le && s[i] != 0 && s[i+1] == 0
			be = be && s[i] == 0 && s[i+1] != 0
		}
		switch {
		case le:
			order = binary.LittleEndian
		case be:
			order = binary.BigEndian
		default:
			return s
		}
	}
	units := make([]uint16, 0, (len(s)+1)/2)
	for i := 0; i+1 < len(s); i += 2 {
		units = append(units, order.Uint16([]byte(s[i:i+2])))
	}
	if len(s)%2 != 0 {
		units = append(units, '\ufffd')
	}
	return string(utf16.Decode(units))
}

func TestGateWaitErrorIsNotCalledAStartFailure(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	fakeGate(t, tools.ShellResult{Output: "partial"}, fmt.Errorf("%w: pipe broke", tools.ErrWait))
	g := runGate(context.Background(), t.TempDir(), "fake gate", time.Minute, nil)
	if g.ExitCode != -1 || strings.Contains(g.InfrastructureError, "could not start") || !strings.Contains(g.InfrastructureError, "pipe broke") {
		t.Fatalf("gate = %+v, want a wait failure", g)
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
		// The beat is replacing this file meanwhile; only readRecord's sharing survives that on Windows.
		data, err := readRecord(path)
		if err == nil {
			err = json.Unmarshal(data, &during)
		}
		if err != nil {
			t.Errorf("record while the gate ran: %v", err)
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
func TestGateUsesLocalTemporaryDirectoryAndCargoDefault(t *testing.T) {
	for _, exit := range []int{0, 1} {
		t.Run(fmt.Sprint(exit), func(t *testing.T) {
			t.Setenv("CARGO_INCREMENTAL", "")
			os.Unsetenv("CARGO_INCREMENTAL")
			dir := t.TempDir()
			tree := filepath.Join(dir, ".hrn", "worktrees", "task")
			if err := os.MkdirAll(tree, 0755); err != nil {
				t.Fatal(err)
			}
			env, cleanup, err := batchEnv(tree)
			if err != nil {
				t.Fatal(err)
			}
			var temp string
			old := runGateShell
			t.Cleanup(func() { runGateShell = old })
			runGateShell = func(_ context.Context, _ string, _ string, _ time.Duration, env []string) (tools.ShellResult, error) {
				vars := map[string]string{}
				for _, kv := range env {
					k, v, _ := strings.Cut(kv, "=")
					vars[strings.ToUpper(k)] = v
				}
				temp = vars["TMP"]
				if vars["CARGO_INCREMENTAL"] != "0" || temp == "" || vars["TEMP"] != temp || vars["TMPDIR"] != temp {
					t.Errorf("unexpected environment: cargo=%q temp=%q", vars["CARGO_INCREMENTAL"], temp)
				}
				if _, err := os.Stat(temp); err != nil {
					t.Errorf("temp during gate: %v", err)
				}
				resolved, err := filepath.EvalSymlinks(temp)
				if err != nil {
					t.Fatal(err)
				}
				root, err := filepath.EvalSymlinks(dir)
				if err != nil {
					t.Fatal(err)
				}
				if filepath.VolumeName(root) != filepath.VolumeName(resolved) {
					t.Errorf("temp on a different volume: %s", resolved)
				}
				return tools.ShellResult{ExitCode: exit}, nil
			}
			runGate(context.Background(), dir, "gate", time.Minute, env)
			if err := cleanup(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(temp); !os.IsNotExist(err) {
				t.Errorf("temp after gate: %v", err)
			}
		})
	}
}

func TestGateTempOutsideGitAncestors(t *testing.T) {
	base := t.TempDir()
	outer := filepath.Join(base, "outer")
	repo := filepath.Join(outer, "nested", "repo")
	tree := filepath.Join(repo, ".hrn", "slots", "1")
	if err := os.MkdirAll(tree, 0755); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{outer, repo} {
		if err := os.WriteFile(filepath.Join(root, ".git"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	env, cleanup, err := batchEnv(tree)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	var temp string
	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); k == "TMP" {
			temp = v
		}
	}
	resolved, err := filepath.EvalSymlinks(temp)
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(filepath.Dir(resolved)) != root || filepath.VolumeName(resolved) != filepath.VolumeName(root) {
		t.Errorf("temp = %q, want a folder under %q on the same volume", resolved, filepath.Join(root, ".hrn-gate-tmp"))
	}
	for path := resolved; ; path = filepath.Dir(path) {
		if _, err := os.Lstat(filepath.Join(path, ".git")); !os.IsNotExist(err) {
			t.Errorf("git entry or inaccessible ancestor at %s: %v", path, err)
		}
		if filepath.Dir(path) == path {
			break
		}
	}
}

func TestGateTempFallsBackWhenParentCannotBeCreated(t *testing.T) {
	base := t.TempDir()
	tree := filepath.Join(base, "repo", ".hrn", "slots", "1")
	if err := os.MkdirAll(tree, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, ".hrn-gate-tmp"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	env, cleanup, err := batchEnv(tree)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); k == "TMP" {
			got, err := filepath.EvalSymlinks(filepath.Dir(v))
			if err != nil {
				t.Fatal(err)
			}
			want, err := filepath.EvalSymlinks(os.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("temp parent = %q, want %q", got, want)
			}
			return
		}
	}
	t.Fatal("TMP missing")
}

func TestGatePreservesUserCargoIncremental(t *testing.T) {
	t.Setenv("CARGO_INCREMENTAL", "1")
	old := runGateShell
	t.Cleanup(func() { runGateShell = old })
	runGateShell = func(_ context.Context, _ string, _ string, _ time.Duration, env []string) (tools.ShellResult, error) {
		value := ""
		for _, kv := range env {
			key, v, _ := strings.Cut(kv, "=")
			if key == "CARGO_INCREMENTAL" || runtime.GOOS == "windows" && strings.EqualFold(key, "CARGO_INCREMENTAL") {
				value = v
			}
		}
		if value != "1" {
			t.Errorf("effective cargo value = %q", value)
		}
		return tools.ShellResult{}, nil
	}
	tree := filepath.Join(t.TempDir(), ".hrn", "worktrees", "task")
	if err := os.MkdirAll(tree, 0755); err != nil {
		t.Fatal(err)
	}
	env, cleanup, err := batchEnv(tree)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	runGate(context.Background(), tree, "gate", time.Minute, env)
}

func TestGateCommandReceivesItsTemporaryDirectoryAndCargoDefault(t *testing.T) {
	t.Setenv("CARGO_INCREMENTAL", "0")
	command := `test "$CARGO_INCREMENTAL" = 0 && test -d "$TMP" && test "$TMP" = "$TEMP" && test "$TMP" = "$TMPDIR" && (cygpath -w "$TMP" 2>/dev/null || printf '%s' "$TMP")`
	if name, _ := tools.Shell(); name == "powershell" {
		command = `if ($env:CARGO_INCREMENTAL -ne '0' -or !(Test-Path $env:TMP) -or $env:TMP -ne $env:TEMP -or $env:TMP -ne $env:TMPDIR) { exit 1 }; Write-Output $env:TMP`
	}
	tree := filepath.Join(t.TempDir(), ".hrn", "worktrees", "task")
	if err := os.MkdirAll(tree, 0755); err != nil {
		t.Fatal(err)
	}
	env, cleanup, err := batchEnv(tree)
	if err != nil {
		t.Fatal(err)
	}
	g := runGate(context.Background(), tree, command, time.Minute, env)
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if g.ExitCode != 0 {
		t.Fatalf("gate: %+v", g)
	}
	if _, err := os.Stat(strings.TrimSpace(g.Output)); !os.IsNotExist(err) {
		t.Errorf("temp after gate: %v", err)
	}
}

func TestBatchEnvironmentRespectsCargoConfiguration(t *testing.T) {
	for _, name := range []string{".cargo/config.toml", ".cargo/config", "Cargo.toml"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CARGO_INCREMENTAL", "")
			os.Unsetenv("CARGO_INCREMENTAL")
			root := t.TempDir()
			tree := filepath.Join(root, ".hrn", "worktrees", "task")
			file := filepath.Join(tree, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
				t.Fatal(err)
			}
			table := "[build]"
			if name == "Cargo.toml" {
				table = "[profile.dev]"
			}
			if err := os.WriteFile(file, []byte(table+"\nincremental = true\n"), 0644); err != nil {
				t.Fatal(err)
			}
			env, cleanup, err := batchEnv(tree)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			for _, kv := range env {
				if strings.HasPrefix(strings.ToUpper(kv), "CARGO_INCREMENTAL=") {
					t.Errorf("cargo config overridden: %s", kv)
				}
			}
		})
	}
}

func TestBatchEnvironmentDefaultsCargoUnlessDevOrTestSettingsExist(t *testing.T) {
	cases := []struct {
		name, file, contents string
		configured           bool
	}{
		{"release", "Cargo.toml", "[profile.release]\nincremental = true\n", false},
		{"empty", "Cargo.toml", "[profile.dev]\nopt-level = 1\n", false},
		{"comment", "Cargo.toml", "[profile.dev]\n# incremental = true\n", false},
		{"dev", "Cargo.toml", "[profile.dev]\nincremental = true\n", true},
		{"test", "Cargo.toml", "[profile.test] # comment\nincremental = false\n", true},
		{"next-table", "Cargo.toml", "[profile.dev]\n[profile.release]\nincremental = true\n", false},
		{"config-build", ".cargo/config.toml", "[build]\nincremental = true\n", true},
		{"config-dev", ".cargo/config", "[profile.dev]\nincremental = true\n", true},
		{"config-test", ".cargo/config", "[profile.test]\nincremental = true\n", true},
		{"manifest-build", "Cargo.toml", "[build]\nincremental = true\n", false},
		{"config-release", ".cargo/config", "[profile.release]\nincremental = true\n", false},
		{"spaced-header", "Cargo.toml", "[ profile . dev ]\nincremental = true\n", true},
		{"spaced-build", ".cargo/config.toml", "[\tbuild ]\nincremental = true\n", true},
		{"array-table-resets", "Cargo.toml", "[profile.dev]\n[[bin]]\nincremental = true\n", false},
		{"package-override", "Cargo.toml", "[profile.dev.package.\"*\"]\nincremental = true\n", true},
		{"dev-lookalike", "Cargo.toml", "[profile.devel]\nincremental = true\n", false},
		{"test-override", "Cargo.toml", "[profile.test.build-override]\nincremental = true\n", true},
		{"dev-dash-lookalike", "Cargo.toml", "[profile.dev-x]\nincremental = true\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CARGO_INCREMENTAL", "")
			os.Unsetenv("CARGO_INCREMENTAL")
			tree := filepath.Join(t.TempDir(), ".hrn", "worktrees", "task")
			file := filepath.Join(tree, filepath.FromSlash(tc.file))
			if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte(tc.contents), 0644); err != nil {
				t.Fatal(err)
			}
			env, cleanup, err := batchEnv(tree)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			value, found := "", false
			for _, kv := range env {
				if k, v, _ := strings.Cut(kv, "="); k == "CARGO_INCREMENTAL" {
					value, found = v, true
				}
			}
			if found == tc.configured || found && value != "0" {
				t.Errorf("configured=%v: env=%v", tc.configured, env)
			}
		})
	}
}

func TestBatchEnvironmentIgnoresUnreadableCargoPaths(t *testing.T) {
	for _, mode := range []string{"not-directory", "directory"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("CARGO_INCREMENTAL", "")
			os.Unsetenv("CARGO_INCREMENTAL")
			tree := filepath.Join(t.TempDir(), ".hrn", "worktrees", "task")
			if err := os.MkdirAll(tree, 0755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(tree, ".cargo")
			if mode == "not-directory" {
				if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(filepath.Join(path, "config"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			env, cleanup, err := batchEnv(tree)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if !slices.Contains(env, "CARGO_INCREMENTAL=0") {
				t.Errorf("env=%v", env)
			}
		})
	}
}
