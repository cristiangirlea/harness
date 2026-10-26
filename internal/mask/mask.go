// Package mask redacts human-facing text only.
package mask

import (
	"encoding/json"
	"html"
	"regexp"
	"sort"
	"strings"
)

const hidden = "••••"
const names = `(?i:(?:[A-Z0-9]+[_-])*(?:KEY|TOKEN|SECRET|PASSWORD|AUTH|CREDENTIALS|COOKIE|SESSION)(?:[_-][A-Z0-9]+)*)`

// Specific forms first prevent partial masking; capture only secrets.
var rules = []*regexp.Regexp{
	// Preserve git@ protocol identities.
	regexp.MustCompile(`(?i:[a-z][a-z0-9+.-]*://)(?:git@|(?P<secret>[^\s/@"'<>]+)@)`),
	regexp.MustCompile(`(?:^|\s)(?:-u|--user|--proxy-user)(?:=|\s+)\\"(?P<secret>[^"'\r\n]*:[^"'\r\n]*?)(?:\\"|$)`),
	regexp.MustCompile(`(?:^|\s)(?:-u|--user|--proxy-user)(?:=|\s+)["'](?P<secret>[^"'\r\n]*:[^"'\r\n]*)(?:["']|$)`),
	regexp.MustCompile(`(?:^|\s)(?:-u|--user|--proxy-user)(?:=|\s+)(?P<secret>[^\s"']+:[^\s"']+)`),
	regexp.MustCompile(`"` + names + `"\s*:\s*"(?P<secret>(?:\\.|[^"\\])*)(?:"|$)`),
	regexp.MustCompile(`(?i:\b(?:Authorization|Proxy-Authorization|x-api-key|Cookie|Set-Cookie))\s*:\s*(?P<secret>[^\r\n"'\\]+)`),
	regexp.MustCompile(`(?i:\bBearer)\s+(?P<secret>(?:\\[^"']|[^\s"'\\,;])+)`),
	regexp.MustCompile(`(?:^|[^A-Za-z0-9_/-])(?P<secret>(?:sk-[A-Za-z0-9_-]+|ghp_[A-Za-z0-9_]+|github_pat_[A-Za-z0-9_]+|xox[baprs]-[A-Za-z0-9-]+|AKIA[A-Z0-9]{16}|eyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+))`),
	// Exclude flag prefixes; flags need exact matching.
	regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])` + names + `\s*=\s*\\"(?P<secret>(?:\\[^"\\]|[^"\\])*)(?:\\"|$)`),
	regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])` + names + `\s*=\s*"(?P<secret>(?:\\.|[^"\\])*)(?:"|$)`),
	regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])` + names + `\s*=\s*'(?P<secret>[^']*)(?:'|$)`),
	regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])` + names + `\s*=\s*(?P<secret>(?:\\[^"']|[^\s"'\\,;])+)`),
	regexp.MustCompile(`(?i:--(?:password|passwd|token|api[-_]key|secret|auth|credentials|cookie|session))\b(?:=|\s+)\\"(?P<secret>(?:\\[^"\\]|[^"\\])*)(?:\\"|$)`),
	regexp.MustCompile(`(?i:--(?:password|passwd|token|api[-_]key|secret|auth|credentials|cookie|session))\b(?:=|\s+)"(?P<secret>(?:\\.|[^"\\])*)(?:"|$)`),
	regexp.MustCompile(`(?i:--(?:password|passwd|token|api[-_]key|secret|auth|credentials|cookie|session))\b(?:=|\s+)'(?P<secret>[^']*)(?:'|$)`),
	regexp.MustCompile(`(?i:--(?:password|passwd|token|api[-_]key|secret|auth|credentials|cookie|session))\b(?:=|\s+)(?P<secret>(?:\\[^"']|[^\s"'\\,;])+)`),
}

// Text masks secrets without shortening s, so callers truncate after; records stay unmasked.
func Text(s string) string {
	for _, rule := range rules {
		s = replace(s, rule)
	}
	return s
}

func replace(s string, rule *regexp.Regexp) string {
	group := 2 * rule.SubexpIndex("secret")
	matches := rule.FindAllStringSubmatchIndex(s, -1)
	// Reverse replacement preserves earlier offsets.
	for i := len(matches) - 1; i >= 0; i-- {
		m := matches[i]
		start, end := m[group], m[group+1]
		if start < 0 {
			continue
		}
		value := s[start:end]
		if value == "" || strings.TrimSpace(value) == hidden || strings.TrimSpace(value) == "[redacted]" {
			continue
		}
		s = s[:start] + hidden + s[end:]
	}
	return s
}

// Mask environment secrets in raw, JSON and HTML forms before network viewers see them.
// Skip values shorter than 12 characters to avoid blanking ordinary words.
func Env(env []string) *strings.Replacer {
	seen := map[string]bool{}
	var values []string
	for _, kv := range env {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || len(value) < 12 || !secretName(name) {
			continue
		}
		quoted, _ := json.Marshal(value) // the form a tool's JSON input holds it in
		inJSON := strings.TrimSuffix(strings.TrimPrefix(string(quoted), `"`), `"`)
		for _, v := range []string{value, inJSON, html.EscapeString(value), strings.ReplaceAll(html.EscapeString(value), "+", "&#43;")} {
			if !seen[v] {
				seen[v] = true
				values = append(values, v)
			}
		}
	}
	if len(values) == 0 {
		return nil
	}
	// The longest first, so a value inside another goes with it.
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	var pairs []string
	for _, v := range values {
		pairs = append(pairs, v, "[redacted]")
	}
	return strings.NewReplacer(pairs...)
}

func secretName(name string) bool {
	n := strings.ToUpper(name)
	for _, w := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL"} {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}
