package tools

import "strings"

// OneLine collapses whitespace (errors are often multi-line JSON) and
// truncates to n runes, for logs, prompts and progress lines.
func OneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		s = string(r[:n]) + "…"
	}
	return s
}
