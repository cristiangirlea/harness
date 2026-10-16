package shape

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const (
	summaryLimit = 28000
	middleError  = "build error in the middle of 28000 bytes\n"
)

var (
	oracleFail     = regexp.MustCompile(`--- FAIL:[^\S\n]+(\S+)`)
	oracleDigits   = regexp.MustCompile(`\d+`)
	oracleNegation = regexp.MustCompile(`(?i)\b(?:not|no|never|only|none|cannot|without)\b`)
	// Quotes end a path chunk, so a JSON sample joined without spaces keeps its strings' chunks.
	oraclePath      = regexp.MustCompile(`[^\s"]*[/\\][^\s"]*`)
	oracleJSONHead  = regexp.MustCompile(`^json: (array of|object with) (\d+) (items|keys)$`)
	oracleTableHead = regexp.MustCompile(`^table: (\d+) rows, (\d+) columns$`)
)

// oracleHead reports whether a generated first line states true counts,
// recounting with encoding/json and plain delimiter counts.
func oracleHead(clean, line string) bool {
	if m := oracleJSONHead.FindStringSubmatch(line); m != nil {
		d := json.NewDecoder(strings.NewReader(clean))
		tok, err := d.Token()
		open, ok := tok.(json.Delim)
		array := m[1] == "array of"
		if err != nil || !ok || (open == '[') != array || array != (m[3] == "items") {
			return false
		}
		n := 0
		for ; d.More(); n++ {
			if open == '{' {
				if _, err := d.Token(); err != nil {
					return false
				}
			}
			var raw json.RawMessage
			if d.Decode(&raw) != nil {
				return false
			}
		}
		if _, err := d.Token(); err != nil {
			return false
		}
		if _, err := d.Token(); err != io.EOF {
			return false
		}
		return strconv.Itoa(n) == m[2]
	}
	m := oracleTableHead.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	var rows []string
	for _, l := range strings.Split(clean, "\n") {
		if strings.TrimSpace(l) != "" {
			rows = append(rows, l)
		}
	}
	cols, _ := strconv.Atoi(m[2])
	for _, sep := range []string{"\t", ","} {
		same := cols >= 2
		for _, l := range rows {
			same = same && strings.Count(l, sep) == cols-1
		}
		if same && strconv.Itoa(len(rows)-1) == m[1] {
			return true
		}
	}
	return false
}

// lostFact checks a summary independently of valid and names the first fact it
// lost or invented, or returns "". Stripping uses the package's own escape
// pattern: the oracle judges what is kept, not how escapes are parsed.
func lostFact(original, summary string) string {
	clean := ansi.ReplaceAllString(original, "")
	if first, rest, _ := strings.Cut(summary, "\n"); oracleJSONHead.MatchString(first) || oracleTableHead.MatchString(first) {
		if !oracleHead(clean, first) {
			return fmt.Sprintf("invented %q", first)
		}
		summary = rest // a recounted first line is the one generated number allowed
	}
	kept := map[string]bool{}
	for _, line := range strings.Split(summary, "\n") {
		kept[line] = true
	}
	for _, line := range strings.Split(clean, "\n") {
		if (strings.Contains(strings.ToLower(line), "error") || strings.Contains(line, "FAIL") || strings.Contains(line, "panic")) && !kept[line] {
			return fmt.Sprintf("dropped %q", line)
		}
	}
	failed := map[string]bool{}
	for _, m := range oracleFail.FindAllStringSubmatch(summary, -1) {
		failed[m[1]] = true
	}
	for _, m := range oracleFail.FindAllStringSubmatch(clean, -1) {
		if !failed[m[1]] {
			return fmt.Sprintf("dropped failing test %q", m[1])
		}
	}
	for _, re := range []*regexp.Regexp{oracleDigits, oracleNegation, oraclePath} {
		present := map[string]bool{}
		for _, text := range []string{original, clean} {
			for _, s := range re.FindAllString(text, -1) {
				present[strings.ToLower(s)] = true
			}
		}
		for _, s := range re.FindAllString(summary, -1) {
			if !present[strings.ToLower(s)] {
				return fmt.Sprintf("invented %q", s)
			}
		}
	}
	return ""
}

// filler returns distinct numbered lines of exactly n bytes, ending in a newline.
func filler(prefix string, n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "%s %06d %s\n", prefix, i, strings.Repeat("x", 80))
	}
	return b.String()[:n-1] + "\n"
}

func buriedFailure() string {
	var b strings.Builder
	for i := 1; i <= 2500; i++ {
		fmt.Fprintf(&b, "=== RUN   TestCase%d\n--- PASS: TestCase%d (0.00s)\n", i, i)
		if i == 1250 {
			b.WriteString("=== RUN   TestBroken\n    broken_test.go:42: got 3 rows, want 4\n    broken_test.go:43: not found: /var/data/rows.db\n--- FAIL: TestBroken (0.01s)\n")
		}
	}
	b.WriteString("FAIL\nFAIL\texample/pkg\t0.512s\n")
	return b.String()
}

func repeatedWithTimestamps() string {
	var b strings.Builder
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&b, "2026-10-16T10:00:%02dZ WARN retrying /var/run/app.sock\n", i)
	}
	b.WriteString("2026-10-16T10:01:00Z ERROR gave up on /var/run/app.sock\n")
	return b.String()
}

func TestSummariesKeepEveryFactOrFallBack(t *testing.T) {
	for _, tc := range []struct {
		name  string
		text  string
		kind  Kind     // what Classify must say, so the row probes the intended path
		keeps bool     // the natural kind must summarize rather than fall back
		facts []string // substrings a true summary of the natural kind must hold
	}{
		{"one failure buried in 5000 passing lines", buriedFailure(), Test, true,
			[]string{"tests: passed=2500 failed=1", "--- FAIL: TestBroken (0.01s)", "    broken_test.go:42: got 3 rows, want 4", "    broken_test.go:43: not found: /var/data/rows.db"}},
		{"same failing name in two packages",
			"=== RUN   TestOpen\n    open_test.go:9: open /tmp/a: no such file\n--- FAIL: TestOpen (0.00s)\nFAIL\nFAIL\texample/a\t0.01s\n=== RUN   TestOpen\n    open_test.go:9: open /tmp/b: no such file\n--- FAIL: TestOpen (0.00s)\nFAIL\nFAIL\texample/b\t0.01s\n",
			Test, false, nil},
		{"negation on a dropped log line",
			"2026-10-16 10:00:00 INFO start\n" + "2026-10-16 10:00:01 INFO tick a\n2026-10-16 10:00:01 INFO tick b\n" + "2026-10-16 10:00:02 INFO cache entry not found, rebuilding\n2026-10-16 10:00:03 INFO no such file /var/cache/app/index\n2026-10-16 10:00:04 INFO watcher runs only on Windows\n" + "2026-10-16 10:00:05 INFO tock a\n2026-10-16 10:00:05 INFO tock b\n" + "2026-10-16 10:00:06 ERROR rebuild failed\n",
			Log, true, []string{"ERROR rebuild failed"}},
		{"negation on a dropped terminal line",
			filler("head", 20000) + "skipped: only on Windows\nopen /etc/app.conf: no such file\nuser not found\n" + filler("tail", 12000),
			Terminal, true, []string{"head 000000", "[... omitted ...]"}},
		{"numbers that differ in one digit",
			"count 2\nexit 1\nexit 1\nexit 11\nexit 11\n127.0.0.1:8081\n127.0.0.1:8081\n127.0.0.1:8082\n",
			Terminal, true, []string{"exit 1\n[repeated 2 times]\nexit 11\n[repeated 2 times]\n127.0.0.1:8081\n[repeated 2 times]\n127.0.0.1:8082"}},
		{"error lines that differ in one digit",
			"INFO count 2\nERROR exit 1\nERROR exit 1\nERROR exit 11\nERROR dial 127.0.0.1:8081\nERROR dial 127.0.0.1:8082\n",
			Log, true, []string{"ERROR exit 1\n[repeated 2 times]\nERROR exit 11\nERROR dial 127.0.0.1:8081\nERROR dial 127.0.0.1:8082"}},
		{"error string wrapped in a log",
			"INFO step 1\nINFO step 2\nERROR open /very/long/path/to/the/config/fi\nle.yaml: no such file or directory\nINFO step 3\n",
			Log, true, []string{"ERROR open /very/long/path/to/the/config/fi\nle.yaml: no such file or directory"}},
		{"error string wrapped in a failing test",
			"counts 0 1\n=== RUN   TestLoad\n    load_test.go:12: unexpected error: open /very/long/path/to/the/fi\n        xture.json: not found\n--- FAIL: TestLoad (0.00s)\nFAIL\n",
			Test, true, []string{"    load_test.go:12: unexpected error: open /very/long/path/to/the/fi\n        xture.json: not found"}},
		{"error string wrapped mid terminal output",
			filler("head", 20000) + "dial tcp 10.0.0.7:5432: connection err\nor: refused\n" + filler("tail", 12000),
			Terminal, true, nil},
		{"colour code inside an error word, short", "build: err\x1b[31mor\x1b[0m in main.go\n", Terminal, true, []string{"build: error in main.go"}},
		{"colour code inside an error word, long",
			filler("head", 20000) + "dial tcp: err\x1b[31mor\x1b[0m: connection refused\n" + filler("tail", 12000),
			Terminal, false, nil},
		{"colour code inside FAIL, long",
			filler("head", 20000) + "--- FA\x1b[31mIL\x1b[0m: TestHidden (0.00s)\n" + filler("tail", 12000),
			Test, false, nil},
		{"coloured go test failure",
			"=== RUN   TestColour\n    colour_test.go:3: want 1\n--- \x1b[31mFAIL\x1b[0m: TestColour (0.00s)\nFAIL\n",
			Test, true, []string{"    colour_test.go:3: want 1", "--- FAIL: TestColour (0.00s)"}},
		{"CRLF test output",
			"=== RUN   TestCRLF\r\n    crlf_test.go:5: got 0, want 1\r\n--- FAIL: TestCRLF (0.00s)\r\nFAIL\r\nFAIL\texample/crlf\t0.01s\r\n",
			Test, true, []string{"    crlf_test.go:5: got 0, want 1\r", "--- FAIL: TestCRLF (0.00s)\r"}},
		{"CRLF log output", "INFO start 1\r\nERROR cannot bind :8080\r\nINFO stop\r\n", Log, true, []string{"ERROR cannot bind :8080\r"}},
		{"NUL byte", "INFO start\nERROR bad byte \x00 in /tmp/in.bin\nINFO stop\n", Log, true, []string{"ERROR bad byte \x00 in /tmp/in.bin"}},
		{"invalid UTF-8", "INFO start\nERROR name \xff\xfe invalid in /tmp/in.txt\nINFO stop\n", Log, true, []string{"ERROR name \xff\xfe invalid in /tmp/in.txt"}},
		{"panic with a goroutine dump in a test",
			"=== RUN   TestHandle\n--- FAIL: TestHandle (0.00s)\npanic: assignment to entry in nil map [recovered]\n\tpanic: assignment to entry in nil map\n\ngoroutine 7 [running]:\ntesting.tRunner.func1.2({0x5a1b20, 0x6c3d10})\n\t/usr/local/go/src/testing/testing.go:1632 +0x230\nexample/app.(*Server).handle(...)\n\t/src/app/server.go:88\ncreated by testing.(*T).Run in goroutine 1\n\t/usr/local/go/src/testing/testing.go:1742 +0x390\nFAIL\texample/app\t0.009s\n",
			Test, true, []string{"--- FAIL: TestHandle (0.00s)", "panic: assignment to entry in nil map [recovered]", "example/app.(*Server).handle(...)", "\t/src/app/server.go:88"}},
		{"panic with a goroutine dump in a terminal",
			"panic: runtime error: index out of range [5] with length 3\n\ngoroutine 1 [running]:\nmain.main()\n\t/app/main.go:12 +0x1d\nexit status 2\n",
			Terminal, true, []string{"panic: runtime error: index out of range [5] with length 3", "\t/app/main.go:12 +0x1d"}},
		// Frames past the two-line context window are dropped: the invariant
		// protects error lines and test diagnostics, not every following frame.
		{"stack trace whose first frame is the useful line",
			"2026-10-16 10:00:00 INFO serving\n2026-10-16 10:00:01 ERROR request failed\npanic: assignment to entry in nil map\n\ngoroutine 12 [running]:\nmain.(*Server).handle(...)\n\t/app/server.go:88 +0x1d\nnet/http.HandlerFunc.ServeHTTP(...)\n\t/usr/local/go/src/net/http/server.go:2220\n",
			Log, true, []string{"2026-10-16 10:00:01 ERROR request failed", "panic: assignment to entry in nil map"}},
		{"log line repeated with a changing timestamp", repeatedWithTimestamps(), Log, true,
			[]string{"2026-10-16T10:00:00Z WARN retrying /var/run/app.sock", "2026-10-16T10:00:59Z WARN retrying /var/run/app.sock", "2026-10-16T10:01:00Z ERROR gave up on /var/run/app.sock"}},
		{"output of exactly the limit", filler("head", 14000) + middleError + filler("tail", summaryLimit-14000-len(middleError)), Terminal, true, []string{middleError}},
		{"output one byte over the limit", filler("out", summaryLimit+1), Terminal, true, []string{"out 000000", "[... omitted ...]"}},
		{"error output one byte over the limit", filler("ERROR", summaryLimit+1), Log, false, nil},
		{"single line longer than the limit", strings.Repeat("y", summaryLimit+1), Terminal, false, nil},
		{"single error line longer than the limit", "ERROR " + strings.Repeat("y", summaryLimit), Log, false, nil},
		{"JSON array of 500 objects", jsonItems(500), JSON, true,
			[]string{"json: array of 500 items\n", `"owner":{"login":"dev","meta":{...}}`}},
		{"JSON whose values hold error lines", strings.Replace(jsonItems(300), `"name": "item-150"`, `"name": "error: disk full in item-150"`, 1), JSON, true,
			[]string{"\n    \"name\": \"error: disk full in item-150\","}},
		{"JSON whose first item holds an error", strings.Replace(jsonItems(300), `"name": "item-0"`, `"name": "panic in item-0"`, 1), JSON, true,
			[]string{"json: array of 300 items\nkeys: \"id\",\"name\",\"tags\",\"owner\"\n    \"name\": \"panic in item-0\","}},
		{"deeply nested JSON", strings.Repeat(`{"a":`, 500) + "1" + strings.Repeat("}", 500), JSON, true,
			[]string{`sample: {"a":{"a":{...}}}`}},
		{"JSON nested past the parser's depth limit", strings.Repeat("[", 20000) + strings.Repeat("]", 20000), Terminal, false, nil},
		{"JSON numbers in exponent form and negative", "[" + strings.Repeat(`{"v": -1.5e-3, "w": 2E+10, "n": -0}, `, 200) + `{"v": 1}]`, JSON, true,
			[]string{`sample: {"v":-1.5e-3,"w":2E+10,"n":-0}`}},
		{"single huge JSON string", `"` + strings.Repeat("x", 40000) + `"`, Terminal, false, nil},
		{"JSON object holding one huge string", `{"blob": "` + strings.Repeat("x", 40000) + `"}`, JSON, false, nil},
		{"go test -json output", goTestJSON, Terminal, true, nil},
		{"table whose omitted middle row holds an error", csvRows(1000, 500), Table, true,
			[]string{"[... omitted ...]\n500,job-500,error: timeout,5000\n[... omitted ...]"}},
		{"CSV with quoted commas", "id,name\n" + strings.Repeat("1,\"Smith, J\"\n2,Lee\n", 300), Terminal, true, nil},
		{"CRLF JSON", strings.ReplaceAll(jsonItems(500), "\n", "\r\n"), JSON, true, []string{"json: array of 500 items"}},
		{"CRLF table", strings.ReplaceAll(csvRows(1000, 500), "\n", "\r\n"), Table, true, []string{"500,job-500,error: timeout,5000\r"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.text); got != tc.kind {
				t.Fatalf("Classify = %v, want %v", got, tc.kind)
			}
			for _, kind := range []Kind{Test, Log, Terminal, JSON, Table} {
				s, ok := Summarize(kind, tc.text)
				if !ok {
					if kind == tc.kind && tc.keeps {
						t.Errorf("kind %v fell back on output it should summarize", kind)
					}
					continue
				}
				if len(s) > summaryLimit {
					t.Errorf("kind %v: summary of %d bytes", kind, len(s))
				}
				if lost := lostFact(tc.text, s); lost != "" {
					t.Errorf("kind %v returned true but %s", kind, lost)
				}
				if kind == tc.kind {
					for _, fact := range tc.facts {
						if !strings.Contains(s, fact) {
							t.Errorf("kind %v lost %q", kind, fact)
						}
					}
				}
			}
		})
	}
}
