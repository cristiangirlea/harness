// Package shape makes conservative, locally checked summaries of tool text.
package shape

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type Kind int

const (
	Other Kind = iota
	Test
	Log
	Terminal
)

var (
	testLine   = regexp.MustCompile(`^(?:go test\b|\s*--- (?:FAIL|PASS):|ok\s|FAIL(?:\s|$)|=== RUN\s)`)
	logLine    = regexp.MustCompile(`(?i)^(?:\d{4}-\d\d-\d\d[T ]\d\d:\d\d:\d\d|\[?(?:TRACE|DEBUG|INFO|WARN|WARNING|ERROR|FATAL)\]?(?:\s|:))`)
	failName   = regexp.MustCompile(`--- FAIL:\s+(\S+)`)
	testMarker = regexp.MustCompile(`^\s*=== (RUN|CONT|PAUSE)\s+(\S+)\s*$`)
	testResult = regexp.MustCompile(`^\s*--- (FAIL|PASS):\s+(\S+)`)
	numbers    = regexp.MustCompile(`[+-]?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?`)
	negations  = regexp.MustCompile(`(?i)\b(?:not|never|no|only)\b`)
	paths      = regexp.MustCompile(`(?:[A-Za-z]:[\\/]|\.{0,2}/|[A-Za-z0-9_.-]+[\\/])[^\s<>"'()]+|\b[A-Za-z0-9_.-]+\.[A-Za-z][A-Za-z0-9]*`)
	ansi       = regexp.MustCompile("\x1b(?:\\[[0-?]*[ -/]*[@-~]|\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|[@-_])")
	counts     = regexp.MustCompile(`^tests: passed=(\d+) failed=(\d+)$`)
	repeats    = regexp.MustCompile(`^\[repeated (\d+) times\]$`)
)

func Classify(text string) Kind {
	if text == "" {
		return Other
	}
	log := false
	for _, line := range strings.Split(ansi.ReplaceAllString(text, ""), "\n") {
		if testLine.MatchString(line) {
			return Test
		}
		log = log || logLine.MatchString(line)
	}
	if log {
		return Log
	}
	return Terminal
}

// Summarize returns false rather than relaxing a failed invariant. In particular,
// computed counts must also occur as numeric tokens in the original text.
func Summarize(kind Kind, text string) (string, bool) {
	lines := strings.Split(ansi.ReplaceAllString(text, ""), "\n")
	var summary string
	switch kind {
	case Test:
		passed, failed := testCounts(lines)
		keep := make([]bool, len(lines))
		failures, ok := testFailures(lines)
		if !ok {
			return "", false
		}
		for _, failure := range failures {
			for _, i := range failure.indices {
				keep[i] = true
			}
		}
		for i, line := range lines {
			if errorLine(line) || strings.HasPrefix(line, "ok ") || strings.HasPrefix(line, "ok\t") || strings.HasPrefix(line, "FAIL") {
				keep[i] = true
			}
		}
		var out []string
		out = append(out, fmt.Sprintf("tests: passed=%d failed=%d", passed, failed))
		for i, line := range lines {
			if keep[i] {
				out = append(out, line)
			}
		}
		summary = strings.Join(out, "\n")
	case Log:
		keep := make([]bool, len(lines))
		for i, line := range lines {
			lower := strings.ToLower(line)
			if errorLine(line) || strings.Contains(lower, "warn") || strings.Contains(lower, "fatal") {
				for j := max(0, i-2); j <= min(len(lines)-1, i+2); j++ {
					keep[j] = true
				}
			}
		}
		// Collapse within each continuous window, never across omitted lines.
		var out []string
		for i := 0; i < len(lines); {
			if !keep[i] {
				i++
				continue
			}
			j := i + 1
			for j < len(lines) && keep[j] {
				j++
			}
			out = append(out, collapse(lines[i:j])...)
			i = j
		}
		summary = strings.Join(out, "\n")
		if summary == "" {
			summary = headTail(collapse(lines))
		}
	case Terminal:
		summary = headTail(collapse(lines))
	default:
		return "", false
	}
	return summary, len(summary) <= 28000 && valid(kind, text, summary)
}

func errorLine(line string) bool {
	return strings.Contains(strings.ToLower(line), "error") || strings.Contains(line, "FAIL") || strings.Contains(line, "panic")
}

func testCounts(lines []string) (passed, failed int) {
	for _, line := range lines {
		if strings.Contains(line, "--- PASS:") {
			passed++
		}
		if failName.MatchString(line) {
			failed++
		}
	}
	return
}

type testFailure struct {
	indices  []int
	messages []string
	failed   bool
}

// A RUN or CONT owns following diagnostics until PAUSE or a result. Keep
// paused tests' earlier diagnostics, and refuse unmarked output while more
// than one test is running: its owner cannot be recovered from plain text.
// Non-verbose Go output also prints diagnostics immediately after FAIL.
func testFailures(lines []string) (map[string]*testFailure, bool) {
	tests := map[string]*testFailure{}
	running := map[string]bool{}
	current := ""
	markedOwner, sawMarker, unassigned := false, false, false
	for i, line := range lines {
		if m := testMarker.FindStringSubmatch(line); m != nil {
			sawMarker = true
			markedOwner = m[1] == "CONT"
			name := m[2]
			if tests[name] == nil {
				tests[name] = &testFailure{}
			}
			tests[name].indices = append(tests[name].indices, i)
			if m[1] == "PAUSE" {
				delete(running, name)
				current = ""
			} else {
				running[name], current = true, name
			}
			continue
		}
		if m := testResult.FindStringSubmatch(line); m != nil {
			markedOwner = false
			name := m[2]
			if tests[name] == nil {
				tests[name] = &testFailure{}
			}
			tests[name].indices = append(tests[name].indices, i)
			delete(running, name)
			current = ""
			if m[1] == "FAIL" {
				if tests[name].failed {
					return nil, false // repeated names cannot identify one test
				}
				tests[name].failed, current = true, name
			}
			continue
		}
		if strings.HasPrefix(line, "FAIL") || strings.HasPrefix(line, "ok ") || strings.HasPrefix(line, "ok\t") || strings.HasPrefix(line, "go test") || counts.MatchString(line) {
			current = ""
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if (len(running) > 1 && !markedOwner) || (len(running) > 0 && !running[current]) {
			return nil, false
		}
		if current != "" {
			tests[current].indices = append(tests[current].indices, i)
			tests[current].messages = append(tests[current].messages, line)
		} else {
			unassigned = true
			for _, test := range tests {
				if !test.failed {
					return nil, false // an unfinished test has no output marker
				}
			}
		}
	}
	for name, test := range tests {
		if !test.failed {
			delete(tests, name)
		}
	}
	if !sawMarker && unassigned && len(tests) > 1 {
		return nil, false // diagnostics before unmarked failures have no owner
	}
	return tests, true
}

func collapse(lines []string) []string {
	var out []string
	for i := 0; i < len(lines); {
		j := i + 1
		for j < len(lines) && lines[j] == lines[i] {
			j++
		}
		out = append(out, lines[i])
		if j-i > 1 {
			out = append(out, fmt.Sprintf("[repeated %d times]", j-i))
		}
		i = j
	}
	return out
}

// Cut on line boundaries so partial paths, numbers and errors cannot appear.
func headTail(lines []string) string {
	text := strings.Join(lines, "\n")
	if len(text) <= 28000 {
		return text
	}
	a, b := 0, len(lines)
	size := 0
	for a < len(lines) && size+len(lines[a])+1 <= 18000 {
		size += len(lines[a]) + 1
		a++
	}
	size = 0
	for b > a && size+len(lines[b-1])+1 <= 9000 {
		b--
		size += len(lines[b]) + 1
	}
	if a == 0 || b == len(lines) {
		return text // the size gate falls back when an edge line cannot fit
	}
	return strings.Join(lines[:a], "\n") + "\n[... omitted ...]\n" + strings.Join(lines[b:], "\n")
}

func tokenSet(re *regexp.Regexp, text string) map[string]bool {
	out := map[string]bool{}
	for _, s := range re.FindAllString(text, -1) {
		out[s] = true
	}
	return out
}

func valid(kind Kind, original, summary string) bool {
	for _, re := range []*regexp.Regexp{numbers, negations, paths} {
		available := tokenSet(re, original)
		for _, s := range re.FindAllString(summary, -1) {
			if !available[s] {
				return false
			}
		}
	}
	origLines := strings.Split(original, "\n")
	sumLines := strings.Split(summary, "\n")
	origSet, sumSet := map[string]bool{}, map[string]bool{}
	for _, line := range origLines {
		origSet[line] = true
	}
	for _, line := range sumLines {
		sumSet[line] = true
		if errorLine(line) && !origSet[line] {
			return false
		}
	}
	for _, line := range origLines {
		if errorLine(line) && !sumSet[line] {
			return false
		}
		if m := failName.FindStringSubmatch(line); m != nil {
			found := false
			for _, n := range failName.FindAllStringSubmatch(summary, -1) {
				found = found || n[1] == m[1]
			}
			if !found {
				return false
			}
		}
	}
	cleanLines := strings.Split(ansi.ReplaceAllString(original, ""), "\n")
	if kind == Test {
		origFailures, origOK := testFailures(cleanLines)
		sumFailures, sumOK := testFailures(sumLines)
		if !origOK || !sumOK || len(origFailures) != len(sumFailures) {
			return false
		}
		for name, failure := range origFailures {
			other := sumFailures[name]
			if other == nil || !slices.Equal(failure.messages, other.messages) {
				return false
			}
		}
	}
	for i, line := range sumLines {
		if m := counts.FindStringSubmatch(line); m != nil {
			p, f := testCounts(cleanLines)
			if kind != Test || line != fmt.Sprintf("tests: passed=%d failed=%d", p, f) {
				return false
			}
		}
		if m := repeats.FindStringSubmatch(line); m != nil {
			if i == 0 {
				return false
			}
			n, err := strconv.Atoi(m[1])
			if err != nil || n < 2 {
				return false
			}
			found := false
			for j := 0; j < len(cleanLines); {
				k := j + 1
				for k < len(cleanLines) && cleanLines[k] == cleanLines[j] {
					k++
				}
				if cleanLines[j] == sumLines[i-1] && k-j == n {
					found = true
				}
				j = k
			}
			if !found {
				return false
			}
		}
	}
	return true
}
