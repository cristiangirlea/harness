package tools

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

const editMatchWorkBudget = 8_000_000

func closestLines(content, old string) string {
	return closestLinesForMatch(content, old, old)
}

func closestLinesForMatch(content, old, matchedOld string) string {
	const missing = "old_string not found in file; "
	fileLines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	oldLines := strings.Split(strings.ReplaceAll(old, "\r\n", "\n"), "\n")
	if len(oldLines) > 1 && oldLines[len(oldLines)-1] == "" {
		oldLines = oldLines[:len(oldLines)-1]
	}
	fileNorm := normalizeEditLines(fileLines)
	oldNorm := normalizeEditLines(oldLines)
	width := min(len(oldLines), len(fileLines))
	bestStart, bestScore, compared := 0, -1.0, 0
	for start := 0; start+width <= len(fileLines); start++ {
		score := 0.0
		for i := 0; i < width; i++ {
			a, b := fileNorm[start+i], oldNorm[i]
			cost := 1 + min(len(a), len(b))
			if cost > editMatchWorkBudget-compared {
				return missing + "the file is too large to search for the closest lines"
			}
			compared += cost
			score += editLineSimilarity(a, b)
		}
		score /= float64(len(oldLines))
		if score > bestScore {
			bestStart, bestScore = start, score
		}
	}
	if bestScore < 0.5 {
		return missing + "no similar lines found"
	}
	a, b := bestStart+1, bestStart+width
	var out strings.Builder
	fmt.Fprintf(&out, "%sthe closest lines are %d-%d:\n", missing, a, b)
	out.WriteString(editMatchExcerpt(fileLines[bestStart:bestStart+width], a))
	whitespaceOnly, difference := true, -1
	for i := range oldLines {
		if i >= width || fileNorm[bestStart+i] != oldNorm[i] {
			whitespaceOnly = false
		}
		if difference < 0 && (i >= width || fileLines[bestStart+i] != oldLines[i]) {
			difference = i
		}
	}
	if difference < 0 {
		fmt.Fprintf(&out, "lines %d-%d match old_string except for the final newline or line endings", a, b)
		fileRaw, oldRaw := strings.Split(content, "\n"), strings.Split(matchedOld, "\n")
		for i := 0; i < width; i++ {
			if editLineEnding(fileRaw, bestStart+i) != editLineEnding(oldRaw, i) {
				fmt.Fprintf(&out, "; first line ending difference at line %d", a+i)
				break
			}
		}
	} else if whitespaceOnly {
		fmt.Fprintf(&out, "lines %d-%d match old_string except for whitespace (indentation, tabs or trailing spaces)", a, b)
		fileQuote, oldQuote := editDifferenceQuotes(fileLines[bestStart+difference], oldLines[difference])
		fmt.Fprintf(&out, "; first whitespace difference at line %d: file has %s, old_string has %s", a+difference, fileQuote, oldQuote)
	} else {
		fileLine := ""
		if difference < width {
			fileLine = fileLines[bestStart+difference]
		}
		fileQuote, oldQuote := editDifferenceQuotes(fileLine, oldLines[difference])
		fmt.Fprintf(&out, "first difference at line %d: file has %s, old_string has %s", a+difference, fileQuote, oldQuote)
	}
	return out.String()
}

func editLineEnding(lines []string, i int) string {
	if i >= len(lines)-1 {
		return ""
	}
	if strings.HasSuffix(lines[i], "\r") {
		return "\r\n"
	}
	return "\n"
}

func editDifferenceQuotes(a, b string) (string, string) {
	difference := 0
	for difference < min(len(a), len(b)) && a[difference] == b[difference] {
		difference++
	}
	return editQuotedLine(a, difference), editQuotedLine(b, difference)
}

func editQuotedLine(line string, difference int) string {
	for budget := 194; budget > 0; budget-- {
		start := max(0, difference-budget/2)
		end := min(len(line), start+budget)
		start = max(0, end-budget)
		for steps := 0; steps < utf8.UTFMax-1 && start > 0 && !utf8.RuneStart(line[start]); steps++ {
			start--
		}
		for steps := 0; steps < utf8.UTFMax-1 && end < len(line) && !utf8.RuneStart(line[end]); steps++ {
			end++
		}
		clipped := line[start:end]
		if start > 0 {
			clipped = "..." + clipped
		}
		if end < len(line) {
			clipped += "..."
		}
		if quoted := strconv.Quote(clipped); len(quoted) <= 200 {
			return quoted
		}
	}
	return `"..."`
}

func normalizeEditLines(lines []string) []string {
	normalized := make([]string, len(lines))
	for i, line := range lines {
		normalized[i] = strings.Join(strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == '\t' }), " ")
	}
	return normalized
}

func editLineSimilarity(a, b string) float64 {
	if a == b {
		return 1
	}
	prefix, suffix := 0, 0
	shorter := min(len(a), len(b))
	for prefix < shorter && a[prefix] == b[prefix] {
		prefix++
	}
	for suffix < shorter-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	return float64(prefix+suffix) / float64(max(len(a), len(b)))
}

func editMatchExcerpt(lines []string, start int) string {
	var out strings.Builder
	shown := 0
	for i, line := range lines {
		formatted := fmt.Sprintf("%d\t%s\n", start+i, line)
		if shown == 40 || len(formatted) > 4096-out.Len() {
			break
		}
		out.WriteString(formatted)
		shown++
	}
	if shown < len(lines) {
		fmt.Fprintf(&out, "(... %d more lines)\n", len(lines)-shown)
	}
	return out.String()
}
