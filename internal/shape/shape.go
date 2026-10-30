// Package shape makes conservative, locally checked summaries of tool text.
package shape

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
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
	timestamp  = regexp.MustCompile(`^\d{4}-\d\d-\d\d[T ]\d\d:\d\d:\d\d`)
	failName   = regexp.MustCompile(`--- FAIL:\s+(\S+)`)
	testMarker = regexp.MustCompile(`^\s*=== (RUN|CONT|PAUSE|NAME)\s+(\S+)\s*$`)
	testResult = regexp.MustCompile(`^\s*--- (FAIL|PASS):\s+(\S+)`)
	numbers    = regexp.MustCompile(`[+-]?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?`)
	digits     = regexp.MustCompile(`\d+`)
	negations  = regexp.MustCompile(`(?i)\b(?:not|never|no|only)\b`)
	paths      = regexp.MustCompile(`(?:[A-Za-z]:[\\/]|\.{0,2}/|[A-Za-z0-9_.-]+[\\/])[^\s<>"'()]+|\b[A-Za-z0-9_.-]+\.[A-Za-z][A-Za-z0-9]*`)
	ansi       = regexp.MustCompile("\x1b(?:\\[[0-?]*[ -/]*[@-~]|\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|[@-_])")
	counts     = regexp.MustCompile(`^tests: passed=(\d+) failed=(\d+)$`)
	repeats    = regexp.MustCompile(`^\[repeated (\d+) times\]$`)
	traceMore  = regexp.MustCompile(`^\[\.\.\. (\d+) more trace lines\]$`)
	heads      = regexp.MustCompile(`^(?:json: (?:array of \d+ items|object with \d+ keys)|table: \d+ rows, \d+ columns)$`)
)

const jsonSpace = " \t\r\n"

// Far beyond any real Go test name; longer ones would make prefix lookups quadratic.
const maxTestName = 1024

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
	if _, _, indices, ok := tableShape(lines); ok && (2*logged <= filled || !logLine.MatchString(lines[indices[0]])) {
		return Table
	}
	if logged > 0 {
		return Log
	}
	return Terminal
}

// Summarize rejects failed invariants, requiring original numeric tokens and exactly recomputed JSON/table headers.
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
		summary = logSummary(lines)
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

func logSummary(lines []string) string {
	keep, omitted := make([]bool, len(lines)), make([]bool, len(lines))
	trace := -1
	for i, line := range lines {
		if timestamp.MatchString(line) {
			trace = -1
		}
		if trace >= 0 && !(i == len(lines)-1 && line == "") {
			trace++
			if trace <= 20 {
				keep[i] = true
			} else {
				omitted[i] = true
			}
		}
		lower := strings.ToLower(line)
		if errorLine(line) || strings.Contains(lower, "warn") || strings.Contains(lower, "fatal") {
			end := i
			if !errorLine(line) {
				end = min(len(lines)-1, i+2)
			}
			for j := max(0, i-2); j <= end; j++ {
				keep[j] = true
			}
		}
		if errorLine(line) {
			trace = 0
		}
	}
	var out []string
	for i := 0; i < len(lines); {
		j := i + 1
		if keep[i] {
			for j < len(lines) && keep[j] {
				j++
			}
			end := j
			if j < len(lines) && omitted[j] && lines[j-1] == lines[j] {
				for end > i && lines[end-1] == lines[j] {
					end--
				}
			}
			out = append(out, collapse(lines[i:end])...)
			out = append(out, lines[end:j]...)
		} else if omitted[i] {
			for j < len(lines) && omitted[j] && !keep[j] {
				j++
			}
			out = append(out, fmt.Sprintf("[... %d more trace lines]", j-i))
		}
		i = j
	}
	return strings.Join(out, "\n")
}

func isJSON(s string) bool {
	return s != "" && (s[0] == '{' || s[0] == '[') && json.Valid([]byte(s))
}

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

// Keep raw slices so numbers and strings retain their exact spelling.
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

func jsonHead(text string) (string, []string) {
	s := strings.Trim(text, jsonSpace)
	if !isJSON(s) {
		return "", nil
	}
	keys, values := jsonMembers(s)
	if s[0] == '[' {
		return fmt.Sprintf("json: array of %d items", len(values)), values
	}
	distinct := map[string]bool{}
	for _, key := range keys {
		var decoded string
		_ = json.Unmarshal([]byte(key), &decoded)
		distinct[decoded] = true
	}
	return fmt.Sprintf("json: object with %d keys", len(distinct)), values
}

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

// Suppress generated error-bearing keys/sample lines so all error lines come verbatim from the original.
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
	}
	var errors []string
	for _, line := range lines {
		if errorLine(line) {
			errors = append(errors, line)
		}
	}
	if sample != "" {
		line := "sample: " + jsonSample(sample, 2)
		if !errorLine(line) && len(strings.Join(out, "\n"))+1+len(line)+len(strings.Join(errors, "\n"))+len(errors) <= 28000 {
			out = append(out, line)
		}
	}
	out = append(out, errors...)
	return strings.Join(out, "\n")
}

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
	failedNames := map[string]int{}
	parents := map[string]bool{}
	flush := func() {
		for name, n := range failedNames {
			if !parents[name] {
				failed += n
			}
		}
		clear(failedNames)
		clear(parents)
	}
	for _, line := range lines {
		if testPackageLine(line) {
			flush()
			continue
		}
		if strings.Contains(line, "--- PASS:") {
			passed++
		}
		if m := failName.FindStringSubmatch(line); m != nil {
			failedNames[m[1]]++
			if len(m[1]) > maxTestName {
				continue
			}
			for i := strings.LastIndex(m[1], "/"); i > 0; i = strings.LastIndex(m[1][:i], "/") {
				parents[m[1][:i]] = true
			}
		}
	}
	flush()
	return
}

func testPackageLine(line string) bool {
	fields := strings.Split(line, "\t")
	if len(fields) < 2 {
		return false
	}
	name := strings.TrimSuffix(fields[1], "\r")
	return (fields[0] == "FAIL" || fields[0] == "ok") && name != "" && !strings.ContainsAny(name, " \r\n")
}

type testFailure struct {
	indices  []int
	messages []string
	failed   bool
}

// Preserve paused diagnostics; reject ambiguous ownership when tests overlap.
// Non-verbose Go output can place diagnostics immediately after FAIL.
func testFailures(lines []string) (map[string]*testFailure, bool) {
	all := map[string]*testFailure{}
	start := 0
	for i, line := range lines {
		if (strings.HasPrefix(line, "FAIL\t") || strings.HasPrefix(line, "ok\t")) && failName.MatchString(line) {
			return nil, false
		}
		if !testPackageLine(line) {
			continue
		}
		pkg := strings.TrimSuffix(strings.Split(line, "\t")[1], "\r")
		part, ok := testFailuresPart(lines[start:i])
		if !ok {
			return nil, false
		}
		for name, failure := range part {
			key := pkg + "." + name
			if all[key] != nil {
				return nil, false
			}
			for j := range failure.indices {
				failure.indices[j] += start
			}
			all[key] = failure
		}
		start = i + 1
	}
	part, ok := testFailuresPart(lines[start:])
	if !ok {
		return nil, false
	}
	for name, failure := range part {
		if all[name] != nil {
			return nil, false
		}
		for j := range failure.indices {
			failure.indices[j] += start
		}
		all[name] = failure
	}
	return all, true
}

func testFailuresPart(lines []string) (map[string]*testFailure, bool) {
	tests := map[string]*testFailure{}
	running := map[string]bool{}
	paused := map[string]bool{}
	waitsOn := map[string]string{}
	current := ""
	markedOwner, sawMarker, unassigned, parallel := false, false, false, false
	var pending []int
	for i, line := range lines {
		if m := testMarker.FindStringSubmatch(line); m != nil {
			pending = nil // text before a RUN is outside that test
			sawMarker = true
			markedOwner = m[1] == "CONT" || m[1] == "NAME"
			name := m[2]
			if len(name) > maxTestName {
				return nil, false
			}
			if tests[name] == nil {
				tests[name] = &testFailure{}
			}
			tests[name].indices = append(tests[name].indices, i)
			parallel = parallel || m[1] == "PAUSE" || m[1] == "CONT"
			if m[1] == "RUN" {
				for i := strings.LastIndex(name, "/"); i > 0; i = strings.LastIndex(name[:i], "/") {
					if paused[name[:i]] {
						return nil, false
					}
				}
			}
			if m[1] == "RUN" && !parallel {
				endSiblings(running, waitsOn, name)
			}
			if m[1] == "PAUSE" {
				paused[name] = true
				delete(running, name)
				current = ""
			} else {
				delete(paused, name)
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
			if len(name) > maxTestName {
				return nil, false
			}
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
				// Leading diagnostics are attributable only when exactly one unmarked test fails.
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
		if !markedOwner {
			for name := range running { // a running parent waits on its subtest, not beside it
				if name != current && !subtestOf(current, name) {
					return nil, false
				}
			}
		}
		if len(running) > 0 && !running[current] {
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
			if running[name] || paused[name] {
				return nil, false
			}
			delete(tests, name)
		}
	}
	if !sawMarker && unassigned && len(tests) > 1 {
		return nil, false // diagnostics before unmarked failures have no owner
	}
	return tests, true
}

func subtestOf(name, parent string) bool {
	return strings.HasPrefix(name, parent+"/")
}

// Sibling RUN marks completion before delayed results; find the longest running prefix because names may contain slashes.
func endSiblings(running map[string]bool, waitsOn map[string]string, name string) {
	parent := ""
	for i := strings.LastIndex(name, "/"); i > 0; i = strings.LastIndex(name[:i], "/") {
		if running[name[:i]] {
			parent = name[:i]
			break
		}
	}
	if parent == "" {
		return
	}
	for child := waitsOn[parent]; child != ""; { // each child name is longer, so the chain ends
		next := waitsOn[child]
		delete(running, child)
		delete(waitsOn, child)
		child = next
	}
	waitsOn[parent] = name
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

func numberTokens(text string) []string {
	var out []string
	for _, loc := range numbers.FindAllStringIndex(text, -1) {
		before, _ := utf8.DecodeLastRuneInString(text[:loc[0]])
		after, _ := utf8.DecodeRuneInString(text[loc[1]:])
		if (loc[0] > 0 && (unicode.IsLetter(before) || unicode.IsDigit(before) || before == '_')) ||
			(loc[1] < len(text) && (unicode.IsLetter(after) || unicode.IsDigit(after) || after == '_')) {
			continue
		}
		out = append(out, text[loc[0]:loc[1]])
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
	availableNumbers := map[string]bool{}
	availableDigits := tokenSet(digits, strings.Join(cleanLines, "\n"))
	for _, n := range numberTokens(strings.Join(cleanLines, "\n")) {
		availableNumbers[n] = true
	}
	for _, line := range strings.Split(body, "\n") {
		if counts.MatchString(line) || repeats.MatchString(line) || traceMore.MatchString(line) {
			continue
		}
		for _, n := range numberTokens(line) {
			if !availableNumbers[n] {
				return false
			}
		}
		for _, n := range digits.FindAllString(line, -1) {
			if !availableDigits[n] {
				return false
			}
		}
	}
	for _, re := range []*regexp.Regexp{negations, paths} {
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
	if kind == Test || failName.MatchString(strings.Join(cleanLines, "\n")) {
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
	checkedTrace := false
	for i, line := range sumLines {
		if traceMore.MatchString(line) && !checkedTrace {
			checkedTrace = true
			if kind != Log || summary != logSummary(cleanLines) {
				return false
			}
		}
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
