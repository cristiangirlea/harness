package shape

import (
	"strings"
	"testing"
)

func FuzzSummarize(f *testing.F) {
	for _, seed := range []string{
		// The adversarial table's attacks, shortened.
		"=== RUN   TestCase1\n--- PASS: TestCase1 (0.00s)\n=== RUN   TestBroken\n    broken_test.go:42: got 3 rows, want 4\n--- FAIL: TestBroken (0.01s)\nFAIL\nFAIL\texample/pkg\t0.512s\n",
		"=== RUN   TestOpen\n--- FAIL: TestOpen (0.00s)\nFAIL\texample/a\t0.01s\n=== RUN   TestOpen\n--- FAIL: TestOpen (0.00s)\nFAIL\texample/b\t0.01s\n",
		"2026-10-16 10:00:02 INFO cache entry not found\n2026-10-16 10:00:04 INFO only on Windows\n2026-10-16 10:00:06 ERROR rebuild failed\n",
		"count 2\nexit 1\nexit 1\nexit 11\n127.0.0.1:8081\n127.0.0.1:8081\n127.0.0.1:8082\n",
		"INFO step 1\nERROR open /very/long/fi\nle.yaml: no such file\nINFO step 3\n",
		"dial tcp 10.0.0.7:5432: connection err\nor: refused\n",
		"build: err\x1b[31mor\x1b[0m in main.go\n",
		"--- FA\x1b[31mIL\x1b[0m: TestHidden (0.00s)\n",
		"=== RUN   TestCRLF\r\n    crlf_test.go:5: got 0, want 1\r\n--- FAIL: TestCRLF (0.00s)\r\nFAIL\r\n",
		"INFO start\nERROR bad byte \x00 in /tmp/in.bin\nERROR name \xff\xfe invalid\n",
		"--- FAIL: TestHandle (0.00s)\npanic: nil map [recovered]\n\ngoroutine 7 [running]:\nexample/app.(*Server).handle(...)\n\t/src/app/server.go:88\nFAIL\texample/app\t0.009s\n",
		"2026-10-16T10:00:00Z WARN retrying /var/run/app.sock\n2026-10-16T10:00:01Z WARN retrying /var/run/app.sock\n",
		// The existing tests' samples.
		"counts 1 2\n=== RUN   TestGood\n--- PASS: TestGood (0.00s)\n=== RUN   TestBad\n    bad_test.go:7: no match\n--- FAIL: TestBad (0.00s)\nFAIL\n",
		"INFO counts 2\nINFO idle\nINFO idle\nWARN retry\nERROR /tmp/x: not ready\nINFO after\n",
		"count 3\n\x1b[32mhello\x1b[0m\nhello\nhello\nend\n",
		"\x1b]0;title\ahello\n",
		"counts 1\n=== RUN   TestBad\n    ordinary diagnostic\n=== PAUSE TestBad\n=== RUN   TestGood\n--- PASS: TestGood (0.00s)\n=== CONT  TestBad\n--- FAIL: TestBad (0.00s)\nFAIL\n",
		"=== CONT  TestBad\n=== CONT  TestGood\n=== NAME  TestBad\n    bad_test.go:1: got empty\n--- FAIL: TestBad (0.00s)\n--- PASS: TestGood (0.00s)\nFAIL\n",
		"--- FAIL: TestA (0.00s)\n    a_test.go:1: got 0 of 2 values\n--- FAIL: TestB (0.00s)\n    b_test.go:2: got missing\nFAIL\n",
		"counts 0 1\n--- FAIL: TestOuter (0.00s)\n    detail: not ready\nFAIL\n",
		"hello\n[repeated 2 times]\nhello\nhello\n",
	} {
		f.Add(seed)
	}
	// Blank lines filling the cut's head and tail budgets put the input in the
	// dropped middle, where valid is the only guard. Only escapes make kept lines
	// differ from the original's, and checking a padded input costs milliseconds.
	head := strings.Repeat(" ", 17999) + "\n"
	tail := "\n" + strings.Repeat(" ", 1001) + "\n" + strings.Repeat(" ", 8999)
	check := func(t *testing.T, kind Kind, text string) {
		s, ok := Summarize(kind, text)
		if !ok {
			return
		}
		if len(s) > summaryLimit {
			t.Fatalf("kind %v: summary of %d bytes", kind, len(s))
		}
		if lost := lostFact(text, s); lost != "" {
			t.Fatalf("kind %v returned true but %s\nsummary: %q", kind, lost, s)
		}
	}
	f.Fuzz(func(t *testing.T, text string) {
		for _, kind := range []Kind{Classify(text), Test, Log, Terminal} {
			check(t, kind, text)
		}
		if strings.Contains(text, "\x1b") {
			check(t, Terminal, head+text+tail)
		}
	})
}
