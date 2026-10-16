package shape

import (
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		text string
		want Kind
	}{
		{"go test ./...", Test}, {"--- FAIL: TestBroken (0.00s)", Test},
		{"--- PASS: TestGood (0.00s)", Test}, {"ok  example/pkg 0.1s", Test}, {"FAIL\texample/pkg", Test},
		{"2026-10-14T10:00:00Z service started", Log}, {"[WARN] retry", Log},
		{"info started", Log},
		{"INFO started\n--- FAIL: TestX (0.00s)", Test}, {"ordinary output", Terminal},
		{"okay this is not a test", Terminal}, {"", Other},
	} {
		if got := Classify(tc.text); got != tc.want {
			t.Errorf("Classify(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestSummarize(t *testing.T) {
	for _, tc := range []struct {
		name          string
		kind          Kind
		text          string
		wants, absent []string
		ok            bool
	}{
		{"test", Test, "counts 1 2\n=== RUN   TestGood\n--- PASS: TestGood (0.00s)\n=== RUN   TestBad\n    bad_test.go:7: no match\n--- FAIL: TestBad (0.00s)\n=== RUN   TestWorse\n    bad_test.go:8: never ready\n--- FAIL: TestWorse (0.00s)\nFAIL\n", []string{"passed=1 failed=2", "TestBad", "bad_test.go:7: no match", "TestWorse", "never ready", "FAIL"}, []string{"--- PASS:", "=== RUN   TestGood"}, true},
		{"log", Log, "INFO counts 2\nINFO idle\nINFO idle\nINFO idle\nINFO before\nWARN retry\nERROR /tmp/x: not ready\nINFO after\nINFO done\nINFO boring\nINFO boring\n", []string{"WARN retry", "ERROR /tmp/x: not ready", "INFO before", "INFO after"}, []string{"INFO boring"}, true},
		{"terminal", Terminal, "count 3\n\x1b[32mhello\x1b[0m\nhello\nhello\nend\n", []string{"hello\n[repeated 3 times]", "end"}, []string{"\x1b["}, true},
		{"osc", Terminal, "\x1b]0;title\ahello\n", []string{"hello"}, []string{"title", "\x1b"}, true},
		{"invented repeat count", Terminal, "hello\nhello\nhello\n", nil, nil, false},
		{"invented test count", Test, "--- PASS: TestGood (0.00s)\n", nil, nil, false},
		{"other", Other, "", nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Summarize(tc.kind, tc.text)
			if ok != tc.ok {
				t.Fatalf("ok=%v want %v: %q", ok, tc.ok, got)
			}
			if !ok {
				return
			}
			for _, want := range tc.wants {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in %q", want, got)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(got, absent) {
					t.Errorf("kept %q in %q", absent, got)
				}
			}
		})
	}
}

func TestInvariantsRejectAdversarialSummaries(t *testing.T) {
	for _, tc := range []struct {
		name              string
		kind              Kind
		original, summary string
	}{
		{"missing failing test with same numbers", Test, "--- FAIL: TestA (0.00s)\n--- FAIL: TestB (0.00s)", "--- FAIL: TestA (0.00s)"},
		{"changed count with all errors", Test, "counts 0 1 2\n--- FAIL: TestA (0.00s)\nerror broken", "tests: passed=0 failed=2\n--- FAIL: TestA (0.00s)\nerror broken"},
		{"negation", Terminal, "ready", "not ready"}, {"never", Terminal, "ready", "never ready"}, {"no", Terminal, "ready", "no ready"}, {"only", Terminal, "ready", "only ready"},
		{"number substring", Terminal, "123", "23"}, {"number", Terminal, "1", "2"},
		{"bare file path", Terminal, "old.go:7", "new.go:7"},
		{"number sign", Terminal, "count 2", "count -2"},
		{"number exponent", Terminal, "count 2 3", "count 2e3"},
		{"path", Terminal, "file /tmp/old", "file /tmp/new"}, {"windows path", Terminal, `C:\work\old.txt`, `C:\work\new.txt`}, {"relative path", Terminal, "src/old.go", "src/new.go"},
		{"path prefix", Terminal, "file /tmp/config.yaml", "file /tmp/config"},
		{"invented error", Log, "INFO ready", "error broken"}, {"missing error", Terminal, "error broken\nready", "ready"},
		{"error substring", Terminal, "error long broken", "error broken"},
		{"error hidden in another line", Log, "error broken", "prefix error broken"},
		{"panic", Terminal, "panic: boom\nready", "ready"},
		{"changed repeat count already present", Terminal, "count 2 3\nx\nx", "count 2 3\nx\n[repeated 3 times]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if valid(tc.kind, tc.original, tc.summary) {
				t.Fatal("accepted an invalid summary")
			}
		})
	}
}

func TestParallelFailureDiagnostics(t *testing.T) {
	text := "counts 1\n=== RUN   TestBad\n    ordinary diagnostic\n=== PAUSE TestBad\n=== RUN   TestGood\n--- PASS: TestGood (0.00s)\n=== CONT  TestBad\n--- FAIL: TestBad (0.00s)\nFAIL\n"
	s, ok := Summarize(Test, text)
	if !ok || !strings.Contains(s, "    ordinary diagnostic") {
		t.Fatalf("parallel diagnostic lost: %q ok=%v", s, ok)
	}
}

func TestFailureDiagnosticsStayWithTheirTest(t *testing.T) {
	original := "counts 0 2\n=== RUN   TestA\n    first message\n    second message\n--- FAIL: TestA (0.00s)\n=== RUN   TestB\n    other message\n--- FAIL: TestB (0.00s)\nFAIL\n"
	if s, ok := Summarize(Test, original); !ok || !valid(Test, original, s) {
		t.Fatalf("ordinary failures rejected: %q ok=%v", s, ok)
	}
	for _, tc := range []struct{ name, summary string }{
		{"swapped", "counts 0 2\n=== RUN   TestA\n    other message\n--- FAIL: TestA (0.00s)\n=== RUN   TestB\n    first message\n    second message\n--- FAIL: TestB (0.00s)\nFAIL\n"},
		{"removed", strings.Replace(original, "    first message\n", "", 1)},
		{"reordered", strings.Replace(original, "    first message\n    second message", "    second message\n    first message", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if valid(Test, original, tc.summary) {
				t.Fatal("accepted changed failure diagnostics")
			}
		})
	}
}

func TestAmbiguousParallelDiagnosticsFallBack(t *testing.T) {
	for _, text := range []string{
		"counts 0 2\n=== RUN   TestA\n=== RUN   TestB\n    unattributed diagnostic\n--- FAIL: TestA (0.00s)\n--- FAIL: TestB (0.00s)\nFAIL\n",
		"counts 0 2\n    first unattributed diagnostic\n    second unattributed diagnostic\n--- FAIL: TestA (0.00s)\n--- FAIL: TestB (0.00s)\nFAIL\n",
	} {
		if _, ok := Summarize(Test, text); ok {
			t.Fatal("accepted diagnostics with ambiguous ownership")
		}
	}
}

func TestSequentialSubtestDiagnosticsStayWithTheSubtest(t *testing.T) {
	text := "counts 0 2\n=== RUN   TestA\n=== RUN   TestA/sub\n    a_test.go:5: got 1\n    a_test.go:6: want 2\n    --- FAIL: TestA/sub (0.00s)\n--- FAIL: TestA (0.00s)\nFAIL\n"
	s, ok := Summarize(Test, text)
	if !ok || !strings.Contains(s, "    a_test.go:5: got 1\n    a_test.go:6: want 2\n    --- FAIL: TestA/sub") {
		t.Fatalf("sequential subtest output fell back: %q ok=%v", s, ok)
	}
	failures, ok := testFailures(strings.Split(text, "\n"))
	if !ok || len(failures["TestA/sub"].messages) != 2 || len(failures["TestA"].messages) != 0 {
		t.Fatalf("subtest diagnostics moved: ok=%v %+v", ok, failures)
	}
	if valid(Test, text, strings.Replace(s, "=== RUN   TestA/sub\n", "", 1)) {
		t.Fatal("accepted a summary that gives the subtest's diagnostics to its parent")
	}
}

func TestParallelSubtestsStillFallBack(t *testing.T) {
	text := "counts 0 3\n=== RUN   TestA\n=== RUN   TestA/one\n=== RUN   TestA/two\n    unattributed diagnostic\n    --- FAIL: TestA/one (0.00s)\n    --- FAIL: TestA/two (0.00s)\n--- FAIL: TestA (0.00s)\nFAIL\n"
	if _, ok := Summarize(Test, text); ok {
		t.Fatal("accepted diagnostics while two sibling subtests ran")
	}
}

func TestDiagnosticsBesideAResumedParallelSiblingFallBack(t *testing.T) {
	text := "counts 0 2\n=== RUN   TestA\n=== RUN   TestA/one\n=== PAUSE TestA/one\n=== RUN   TestA/two\n=== PAUSE TestA/two\n" +
		"=== CONT  TestA/one\n=== CONT  TestA/two\n=== RUN   TestA/one/x\n    whose diagnostic\n" +
		"        --- FAIL: TestA/one/x (0.00s)\n    --- FAIL: TestA/one (0.00s)\n    --- PASS: TestA/two (0.00s)\n--- FAIL: TestA (0.00s)\nFAIL\n"
	if _, ok := Summarize(Test, text); ok {
		t.Fatal("accepted unmarked text while a parallel sibling ran")
	}
}

func TestAPausedParallelChildLeavesASequentialSiblingItsDiagnostics(t *testing.T) {
	text := "counts 1 2\n=== RUN   TestA\n=== RUN   TestA/one\n=== PAUSE TestA/one\n=== RUN   TestA/two\n    a_test.go:9: got 3\n" +
		"=== CONT  TestA/one\n    --- PASS: TestA/one (0.00s)\n    --- FAIL: TestA/two (0.00s)\n--- FAIL: TestA (0.00s)\nFAIL\n"
	failures, ok := testFailures(strings.Split(text, "\n"))
	if !ok || failures["TestA/two"] == nil || len(failures["TestA/two"].messages) != 1 || len(failures["TestA"].messages) != 0 {
		t.Fatalf("the sibling's diagnostics were not its own: ok=%v %+v", ok, failures)
	}
	if s, ok := Summarize(Test, text); !ok || !strings.Contains(s, "a_test.go:9: got 3") {
		t.Fatalf("fell back: %q ok=%v", s, ok)
	}
}

func TestParallelFailuresWithContextMarkers(t *testing.T) {
	text := "counts 0 2\n=== RUN   TestA\n=== PAUSE TestA\n=== RUN   TestB\n=== PAUSE TestB\n=== CONT  TestA\n    first message\n=== CONT  TestB\n    second message\n--- FAIL: TestB (0.00s)\n=== CONT  TestA\n    final message\n--- FAIL: TestA (0.00s)\nFAIL\n"
	if s, ok := Summarize(Test, text); !ok || !strings.Contains(s, "    first message") || !strings.Contains(s, "    second message") || !strings.Contains(s, "    final message") {
		t.Fatalf("marked parallel failures rejected: %q ok=%v", s, ok)
	}
}

func TestFailureDiagnosticsWithNameMarker(t *testing.T) {
	text := "=== CONT  TestBad\n=== CONT  TestGood\n=== NAME  TestBad\n    bad_test.go:1: got empty\n--- FAIL: TestBad (0.00s)\n--- PASS: TestGood (0.00s)\nFAIL\n"
	want := "tests: passed=1 failed=1\n=== CONT  TestBad\n=== NAME  TestBad\n    bad_test.go:1: got empty\n--- FAIL: TestBad (0.00s)\nFAIL"
	if s, ok := Summarize(Test, text); !ok || s != want {
		t.Fatalf("NAME diagnostic lost: %q ok=%v", s, ok)
	}
	if valid(Test, text, strings.Replace(want, "    bad_test.go:1: got empty\n", "", 1)) {
		t.Fatal("accepted a removed NAME diagnostic")
	}
}

func TestUnknownTestMarkersFallBack(t *testing.T) {
	for _, marker := range []string{"=== SWITCH TestGood", "=== FUTURE", "=== FUTURE TestBad extra"} {
		t.Run(marker, func(t *testing.T) {
			text := "counts 1\n=== CONT  TestBad\n=== CONT  TestGood\n" + marker + "\n    bad_test.go:1: got empty\n--- FAIL: TestBad (0.00s)\n--- PASS: TestGood (0.00s)\nFAIL\n"
			if _, ok := Summarize(Test, text); ok {
				t.Fatal("accepted an unknown ownership marker")
			}
			if valid(Test, text, "tests: passed=1 failed=1\n=== CONT  TestBad\n--- FAIL: TestBad (0.00s)\nFAIL") {
				t.Fatal("validator accepted an unknown ownership marker")
			}
		})
	}
}

func TestNonVerboseFailureDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
	}{
		{"two diagnostics", "--- FAIL: TestA (0.00s)\n    a_test.go:1: got 0 of 2 values\n--- FAIL: TestB (0.00s)\n    b_test.go:2: got missing\nFAIL\n", "tests: passed=0 failed=2\n--- FAIL: TestA (0.00s)\n    a_test.go:1: got 0 of 2 values\n--- FAIL: TestB (0.00s)\n    b_test.go:2: got missing\nFAIL"},
		{"silent first", "--- FAIL: TestA (0.00s)\n--- FAIL: TestB (0.00s)\n    b_test.go:2: got 0 values\nFAIL\n", "tests: passed=0 failed=2\n--- FAIL: TestA (0.00s)\n--- FAIL: TestB (0.00s)\n    b_test.go:2: got 0 values\nFAIL"},
		{"silent second", "--- FAIL: TestA (0.00s)\n    a_test.go:2: got 0 values\n--- FAIL: TestB (0.00s)\nFAIL\n", "tests: passed=0 failed=2\n--- FAIL: TestA (0.00s)\n    a_test.go:2: got 0 values\n--- FAIL: TestB (0.00s)\nFAIL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if s, ok := Summarize(Test, tc.text); !ok || s != tc.want {
				t.Fatalf("non-verbose diagnostics changed: %q ok=%v", s, ok)
			}
			for _, line := range strings.Split(tc.text, "\n") {
				if strings.HasPrefix(line, "    ") && valid(Test, tc.text, strings.Replace(tc.want, line+"\n", "", 1)) {
					t.Fatalf("accepted a removed diagnostic: %q", line)
				}
			}
		})
	}
}

func TestUnmarkedFailurePreservesLeadingDiagnostics(t *testing.T) {
	text := "counts 0 1\n    first diagnostic\n    second diagnostic\n--- FAIL: TestBad (0.00s)\nFAIL\n"
	s, ok := Summarize(Test, text)
	if !ok || !strings.Contains(s, "    first diagnostic") || !strings.Contains(s, "    second diagnostic") {
		t.Fatalf("unmarked diagnostic lost: %q ok=%v", s, ok)
	}
	if valid(Test, text, strings.Replace(text, "    first diagnostic\n", "", 1)) {
		t.Fatal("accepted a removed unmarked diagnostic")
	}
}

func TestTerminalHeadTailAndErrorFallback(t *testing.T) {
	text := strings.Repeat("head\n", 1) + strings.Repeat(strings.Repeat("x", 100)+"\n"+strings.Repeat("y", 100)+"\n", 250) + "tail"
	s, ok := Summarize(Terminal, text)
	if !ok || len(s) >= len(text) || !strings.HasPrefix(s, "head") || !strings.HasSuffix(s, "tail") {
		t.Fatalf("head/tail: ok=%v length=%d", ok, len(s))
	}
	text = text[:len(text)/2] + "\nerror in the middle\n" + text[len(text)/2:]
	if _, ok := Summarize(Terminal, text); ok {
		t.Fatal("dropped a middle error")
	}
}

func TestFailureMessagesAfterMarker(t *testing.T) {
	text := "counts 0 1\n--- FAIL: TestOuter (0.00s)\n    detail: not ready\n    more details\nFAIL\n"
	s, ok := Summarize(Test, text)
	if !ok || !strings.Contains(s, "detail: not ready") || !strings.Contains(s, "more details") {
		t.Fatalf("failure message lost: %q ok=%v", s, ok)
	}
}

func TestLogCollapsesRepeatedErrorLines(t *testing.T) {
	text := "INFO count 3\nERROR broken\nERROR broken\nERROR broken\n"
	s, ok := Summarize(Log, text)
	if !ok || strings.Count(s, "ERROR broken") != 1 || !strings.Contains(s, "[repeated 3 times]") {
		t.Fatalf("log repetition: %q ok=%v", s, ok)
	}
}

func TestSingleLongLineFallsBack(t *testing.T) {
	if _, ok := Summarize(Terminal, strings.Repeat("x", 50000)); ok {
		t.Fatal("accepted a summary that drops both head and tail")
	}
}
