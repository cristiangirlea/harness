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
		{"path", Terminal, "file /tmp/old", "file /tmp/new"}, {"windows path", Terminal, `C:\work\old.txt`, `C:\work\new.txt`}, {"relative path", Terminal, "src/old.go", "src/new.go"},
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
