// Package shape makes conservative, locally checked summaries of tool text.
package shape

import (
	"fmt"
	"regexp"
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
	testLine  = regexp.MustCompile(`^(?:go test\b|\s*--- (?:FAIL|PASS):|ok\s|FAIL(?:\s|$)|=== RUN\s)`)
	logLine   = regexp.MustCompile(`(?i)^(?:\d{4}-\d\d-\d\d[T ]\d\d:\d\d:\d\d|\[?(?:TRACE|DEBUG|INFO|WARN|WARNING|ERROR|FATAL)\]?(?:\s|:))`)
	failName  = regexp.MustCompile(`--- FAIL:\s+(\S+)`)
	numbers   = regexp.MustCompile(`[+-]?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?`)
	negations = regexp.MustCompile(`(?i)\b(?:not|never|no|only)\b`)
	paths     = regexp.MustCompile(`(?:[A-Za-z]:[\\/]|\.{0,2}/|[A-Za-z0-9_.-]+[\\/])[^\s<>"'()]+|\b[A-Za-z0-9_.-]+\.[A-Za-z][A-Za-z0-9]*`)
	ansi      = regexp.MustCompile("\x1b(?:\\[[0-?]*[ -/]*[@-~]|\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|[@-_])")
	counts    = regexp.MustCompile(`^tests: passed=(\d+) failed=(\d+)$`)
	repeats   = regexp.MustCompile(`^\[repeated (\d+) times\]$`)
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
		start := 0
		afterFail := false
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "=== RUN") {
				start = i
				afterFail = false
			}
			if afterFail {
				keep[i] = true
			}
			if failName.MatchString(line) {
				for j := start; j <= i; j++ {
					keep[j] = true
				}
			}
			if strings.Contains(line, "--- PASS:") || failName.MatchString(line) {
				start = i + 1
				afterFail = failName.MatchString(line)
			}
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
