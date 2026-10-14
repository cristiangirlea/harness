// Package mask removes secret-looking values from human-facing text only.
package mask

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

const hidden = "••••"
const names = `(?i:[A-Z0-9_-]*(?:KEY|TOKEN|SECRET|PASSWORD|AUTH|CREDENTIALS|COOKIE|SESSION)[A-Z0-9_-]*)`

// Each expression names only the secret value; punctuation and field names stay.
// Specific forms precede generic token, assignment and flag rules.
var rules = []*regexp.Regexp{
	regexp.MustCompile(`(?i:[a-z][a-z0-9+.-]*://)(?P<secret>[^\s/@"'<>]+)@`),
	regexp.MustCompile(`(?:^|\s)(?:-u|--user|--proxy-user)(?:=|\s+)\\"(?P<secret>[^"'\r\n]*:[^"'\r\n]*?)(?:\\"|$)`),
	regexp.MustCompile(`(?:^|\s)(?:-u|--user|--proxy-user)(?:=|\s+)["'](?P<secret>[^"'\r\n]*:[^"'\r\n]*)(?:["']|$)`),
	regexp.MustCompile(`(?:^|\s)(?:-u|--user|--proxy-user)(?:=|\s+)(?P<secret>[^\s"']+:[^\s"']+)`),
	regexp.MustCompile(`"` + names + `"\s*:\s*"(?P<secret>(?:\\.|[^"\\])*)(?:"|$)`),
	regexp.MustCompile(`(?i:\b(?:Authorization|Proxy-Authorization|x-api-key|Cookie|Set-Cookie))\s*:\s*(?P<secret>[^\r\n"'\\]+)`),
	regexp.MustCompile(`(?i:\bBearer)\s+(?P<secret>(?:\\[^"']|[^\s"'\\,;])+)`),
	regexp.MustCompile(`(?:^|[^A-Za-z0-9_/-])(?P<secret>(?:sk-[A-Za-z0-9_-]+|ghp_[A-Za-z0-9_]+|github_pat_[A-Za-z0-9_]+|xox[baprs]-[A-Za-z0-9-]+|AKIA[A-Z0-9]{16}|eyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+))`),
	regexp.MustCompile(`(?:^|[^A-Za-z0-9_])` + names + `\s*=\s*\\"(?P<secret>(?:\\[^"\\]|[^"\\])*)(?:\\"|$)`),
	regexp.MustCompile(`(?:^|[^A-Za-z0-9_])` + names + `\s*=\s*"(?P<secret>(?:\\.|[^"\\])*)(?:"|$)`),
	regexp.MustCompile(`(?:^|[^A-Za-z0-9_])` + names + `\s*=\s*'(?P<secret>[^']*)(?:'|$)`),
	regexp.MustCompile(`(?:^|[^A-Za-z0-9_])` + names + `\s*=\s*(?P<secret>(?:\\[^"']|[^\s"'\\,;])+)`),
	regexp.MustCompile(`(?i:--(?:password|passwd|token|api[-_]key|secret|auth|credentials|cookie|session))\b(?:=|\s+)\\"(?P<secret>(?:\\[^"\\]|[^"\\])*)(?:\\"|$)`),
	regexp.MustCompile(`(?i:--(?:password|passwd|token|api[-_]key|secret|auth|credentials|cookie|session))\b(?:=|\s+)"(?P<secret>(?:\\.|[^"\\])*)(?:"|$)`),
	regexp.MustCompile(`(?i:--(?:password|passwd|token|api[-_]key|secret|auth|credentials|cookie|session))\b(?:=|\s+)'(?P<secret>[^']*)(?:'|$)`),
	regexp.MustCompile(`(?i:--(?:password|passwd|token|api[-_]key|secret|auth|credentials|cookie|session))\b(?:=|\s+)(?P<secret>(?:\\[^"']|[^\s"'\\,;])+)`),
}

var cutToken = regexp.MustCompile(`(?:^|[^A-Za-z0-9_/-])(?P<secret>(?:AKIA[A-Za-z0-9]*|eyJ[A-Za-z0-9_.-]*))$`)

var cutURL = regexp.MustCompile(`(?i:[a-z][a-z0-9+.-]*://)(?P<secret>[^\s/@"'<>]+:[^\s/@"'<>]*)$`)

// Text scans at most the first 1 KiB, on a rune boundary. Values reaching the
// boundary are masked as well, before any caller shortens the resulting text.
// The ellipsis marks omitted input; records must never be passed back to storage.
func Text(s string) string {
	cut := len(s) > 1024
	if cut {
		n := 1024
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s = s[:n]
	}
	for i, rule := range rules {
		s = replace(s, rule)
		if cut && i == 0 {
			s = replace(s, cutURL)
		}
		if cut && i == 7 {
			s = replace(s, cutToken)
		}
	}
	if cut {
		s += "…"
	}
	return s
}

func replace(s string, rule *regexp.Regexp) string {
	group := 2 * rule.SubexpIndex("secret")
	matches := rule.FindAllStringSubmatchIndex(s, -1)
	// Work backwards so replacement lengths cannot change earlier offsets.
	for i := len(matches) - 1; i >= 0; i-- {
		m := matches[i]
		start, end := m[group], m[group+1]
		value := s[start:end]
		if value == "" || strings.TrimSpace(value) == hidden || strings.TrimSpace(value) == "[redacted]" {
			continue
		}
		s = s[:start] + hidden + s[end:]
	}
	return s
}
