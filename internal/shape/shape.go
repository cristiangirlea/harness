// Package shape makes conservative, locally checked summaries of tool text.
package shape

import (
	"encoding/json"
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
	JSON
	Table
)

var (
	testLine   = regexp.MustCompile(`^(?:go test\b|\s*--- (?:FAIL|PASS):|ok\s|FAIL(?:\s|$)|=== RUN\s)`)
	logLine    = regexp.MustCompile(`(?i)^(?:\d{4}-\d\d-\d\d[T ]\d\d:\d\d:\d\d|\[?(?:TRACE|DEBUG|INFO|WARN|WARNING|ERROR|FATAL)\]?(?:\s|:))`)
	failName   = regexp.MustCompile(`--- FAIL:\s+(\S+)`)
	testMarker = regexp.MustCompile(`^\s*=== (RUN|CONT|PAUSE|NAME)\s+(\S+)\s*$`)
	testResult = regexp.MustCompile(`^\s*--- (FAIL|PASS):\s+(\S+)`)
	numbers    = regexp.MustCompile(`[+-]?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?`)
	negations  = regexp.MustCompile(`(?i)\b(?:not|never|no|only)\b`)
	paths      = regexp.MustCompile(`(?:[A-Za-z]:[\\/]|\.{0,2}/|[A-Za-z0-9_.-]+[\\/])[^\s<>"'()]+|\b[A-Za-z0-9_.-]+\.[A-Za-z][A-Za-z0-9]*`)
	ansi       = regexp.MustCompile("\x1b(?:\\[[0-?]*[ -/]*[@-~]|\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|[@-_])")
	counts     = regexp.MustCompile(`^tests: passed=(\d+) failed=(\d+)$`)
	repeats    = regexp.MustCompile(`^\[repeated (\d+) times\]$`)
	heads      = regexp.MustCompile(`^(?:json: (?:array of \d+ items|object with \d+ keys)|table: \d+ rows, \d+ columns)$`)
)

const jsonSpace = " \t\r\n"

func Classify(text string) Kind {
	if text == "" {
		return Other
	}
	if isJSON(strings.Trim(text, jsonSpace)) {
		return JSON
	}
	lines := strings.Split(ansi.ReplaceAllString(text, ""), "\n")
	logged, filled := 0, 0
	for _, line := range lines {
		if testLine.MatchString(line) {
			return Test
		}
		if logLine.MatchString(line) {
			logged++
		}
		if strings.TrimSpace(line) != "" {
			filled++
		}
	}
	// Comma timestamps make every log line one column wider; the log summary keeps warnings.
	if _, _, _, ok := tableShape(lines); ok && 2*logged <= filled {
		return Table
	}
	if logged > 0 {
		return Log
	}
	return Terminal
}

// Summarize returns false rather than relaxing a failed invariant. Test counts
// must also occur as numeric tokens in the original; a JSON or table first line
// must equal the one recomputed from it.
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
	case JSON:
		summary = jsonSummary(text, lines)
	case Table:
		summary = tableSummary(lines)
	default:
		return "", false
	}
	if (kind == JSON || kind == Table) && (summary == "" || len(summary) >= len(text)) {
		return "", false
	}
	return summary, len(summary) <= 28000 && valid(kind, text, summary)
}

func isJSON(s string) bool {
	return s != "" && (s[0] == '{' || s[0] == '[') && json.Valid([]byte(s))
}

// jsonEnd returns the index just past the value starting at s[i]; s must be valid JSON.
func jsonEnd(s string, i int) int {
	switch s[i] {
	case '"':
		for i++; s[i] != '"'; i++ {
			if s[i] == '\\' {
				i++
			}
		}
		return i + 1
	case '{', '[':
		for depth := 0; ; i++ {
			switch s[i] {
			case '"':
				i = jsonEnd(s, i) - 1
			case '{', '[':
				depth++
			case '}', ']':
				if depth--; depth == 0 {
					return i + 1
				}
			}
		}
	}
	for i < len(s) && !strings.ContainsRune(",]}"+jsonSpace, rune(s[i])) {
		i++
	}
	return i
}

func skipSpace(s string, i int) int {
	for i < len(s) && strings.IndexByte(jsonSpace, s[i]) >= 0 {
		i++
	}
	return i
}

// jsonMembers returns the raw keys (empty in arrays) and values of a container,
// as byte slices of the original so numbers and strings keep their spelling.
func jsonMembers(s string) (keys, values []string) {
	for i := skipSpace(s, 1); s[i] != '}' && s[i] != ']'; {
		key := ""
		if s[0] == '{' {
			end := jsonEnd(s, i)
			key, i = s[i:end], skipSpace(s, skipSpace(s, end)+1)
		}
		end := jsonEnd(s, i)
		keys, values = append(keys, key), append(values, s[i:end])
		if i = skipSpace(s, end); s[i] == ',' {
			i = skipSpace(s, i+1)
		}
	}
	return keys, values
}

// jsonHead also returns the top-level values, so jsonSummary takes its sample without a second walk.
func jsonHead(text string) (string, []string) {
	s := strings.Trim(text, jsonSpace)
	if !isJSON(s) {
		return "", nil
	}
	_, values := jsonMembers(s)
	if s[0] == '[' {
		return fmt.Sprintf("json: array of %d items", len(values)), values
	}
	return fmt.Sprintf("json: object with %d keys", len(values)), values
}

// jsonSample re-joins raw members without spaces and replaces non-empty
// containers deeper than depth with {...} or [...].
func jsonSample(v string, depth int) string {
	if v[0] != '{' && v[0] != '[' {
		return v
	}
	if skipSpace(v, 1) == len(v)-1 {
		return v[:1] + v[len(v)-1:]
	}
	if depth == 0 {
		return v[:1] + "..." + v[len(v)-1:]
	}
	keys, values := jsonMembers(v)
	var b strings.Builder
	b.WriteByte(v[0])
	for i, value := range values {
		if i > 0 {
			b.WriteByte(',')
		}
		if keys[i] != "" {
			b.WriteString(keys[i])
			b.WriteByte(':')
		}
		b.WriteString(jsonSample(value, depth-1))
	}
	b.WriteByte(v[len(v)-1])
	return b.String()
}

// jsonSummary leaves out a keys or sample line naming an error, since it would be
// an error line absent from the original; the original's error lines follow verbatim.
func jsonSummary(text string, lines []string) string {
	s := strings.Trim(text, jsonSpace)
	head, values := jsonHead(s)
	if head == "" {
		return ""
	}
	out := []string{head}
	sample := s
	if s[0] == '[' {
		sample = ""
		if len(values) > 0 {
			sample = values[0]
		}
	}
	if sample != "" {
		if sample[0] == '{' {
			keys, _ := jsonMembers(sample)
			if line := "keys: " + strings.Join(keys, ","); len(keys) > 0 && !errorLine(line) {
				out = append(out, line)
			}
		}
		if line := "sample: " + jsonSample(sample, 2); !errorLine(line) {
			out = append(out, line)
		}
	}
	for _, line := range lines {
		if errorLine(line) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// tableShape needs five non-blank lines with one count of tabs, or else of
// commas; quoted delimiters break the count, and NDJSON is not a table.
func tableShape(lines []string) (rows, columns int, filled []int, ok bool) {
	for i, line := range lines {
		if strings.TrimSpace(line) != "" {
			filled = append(filled, i)
		}
	}
	if len(filled) < 5 {
		return 0, 0, nil, false
	}
	for _, sep := range []string{"\t", ","} {
		n := strings.Count(lines[filled[0]], sep)
		same := n > 0
		for _, i := range filled {
			same = same && strings.Count(lines[i], sep) == n
		}
		if same && !slices.ContainsFunc(filled, func(i int) bool { return !isJSON(strings.Trim(lines[i], jsonSpace)) }) {
			return 0, 0, nil, false
		}
		if same {
			return len(filled) - 1, n + 1, filled, true
		}
	}
	return 0, 0, nil, false
}

func tableHead(lines []string) string {
	rows, columns, _, ok := tableShape(lines)
	if !ok {
		return ""
	}
	return fmt.Sprintf("table: %d rows, %d columns", rows, columns)
}

func tableSummary(lines []string) string {
	_, _, filled, ok := tableShape(lines)
	if !ok {
		return ""
	}
	keep := make([]bool, len(lines))
	for n, i := range filled {
		keep[i] = n <= 3 || n >= len(filled)-2 || errorLine(lines[i])
	}
	out := []string{tableHead(lines)}
	gap := false
	for i, line := range lines {
		if keep[i] {
			if gap {
				out = append(out, "[... omitted ...]")
			}
			out, gap = append(out, line), false
		} else if strings.TrimSpace(line) != "" {
			gap = true
		}
	}
	return strings.Join(out, "\n")
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

// A RUN, CONT or NAME owns following diagnostics until PAUSE or a result. Keep
// paused tests' earlier diagnostics, and refuse unmarked output while a test
// other than its parents runs beside it: plain text cannot name the owner.
// Non-verbose Go output also prints diagnostics immediately after FAIL.
func testFailures(lines []string) (map[string]*testFailure, bool) {
	tests := map[string]*testFailure{}
	running := map[string]bool{}
	current := ""
	markedOwner, sawMarker, unassigned := false, false, false
	var pending []int
	for i, line := range lines {
		if m := testMarker.FindStringSubmatch(line); m != nil {
			pending = nil // text before a RUN is outside that test
			sawMarker = true
			markedOwner = m[1] == "CONT" || m[1] == "NAME"
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
		if strings.HasPrefix(strings.TrimSpace(line), "=== ") {
			return nil, false // unknown markers may switch diagnostic ownership
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
				// With one unmarked failure, leading diagnostics belong to
				// that test. Multiple failures with such text are rejected below.
				for _, j := range pending {
					tests[name].indices = append(tests[name].indices, j)
					tests[name].messages = append(tests[name].messages, lines[j])
				}
			}
			pending = nil
			continue
		}
		if strings.HasPrefix(line, "FAIL") || strings.HasPrefix(line, "ok ") || strings.HasPrefix(line, "ok\t") || strings.HasPrefix(line, "go test") || counts.MatchString(line) {
			current = ""
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if (len(running) > 1 && !markedOwner && !nested(running, current)) || (len(running) > 0 && !running[current]) {
			return nil, false
		}
		if current != "" {
			tests[current].indices = append(tests[current].indices, i)
			tests[current].messages = append(tests[current].messages, line)
		} else {
			unassigned = true
			pending = append(pending, i)
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

// nested reports whether every running test but current is one of its
// parents: a subtest running inside them, not tests running side by side.
func nested(running map[string]bool, current string) bool {
	for name := range running {
		if name != current && !strings.HasPrefix(current, name+"/") {
			return false
		}
	}
	return true
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
	// Summaries are built from stripped lines; an escape inside a word must not hide it.
	cleanLines := strings.Split(ansi.ReplaceAllString(original, ""), "\n")
	sumLines := strings.Split(summary, "\n")
	// Only a first line recomputed exactly from the original may hold numbers it lacks.
	head, body := "", summary
	switch kind {
	case JSON:
		head, _ = jsonHead(original)
	case Table:
		head = tableHead(cleanLines)
	}
	if head != "" && sumLines[0] == head {
		body = summary[len(head):]
	}
	for _, re := range []*regexp.Regexp{numbers, negations, paths} {
		available := tokenSet(re, original)
		for _, s := range re.FindAllString(body, -1) {
			if !available[s] {
				return false
			}
		}
	}
	origSet, sumSet := map[string]bool{}, map[string]bool{}
	for _, line := range cleanLines {
		origSet[line] = true
	}
	for _, line := range sumLines {
		sumSet[line] = true
		if errorLine(line) && !origSet[line] {
			return false
		}
	}
	for _, line := range cleanLines {
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
		if heads.MatchString(line) && (i != 0 || line != head) {
			return false
		}
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
