package watch

import (
	"html"
	"sort"
	"strings"
)

// secretReplacer blanks the values of secret-looking variables in env (names
// with KEY, TOKEN, SECRET or PASSWORD in them) wherever they appear on a
// page. A run records whatever its agent saw, and an agent that ran env or
// printed a config file would put a key into it; the page may be open to the
// network. Values shorter than 12 characters are left alone: they would
// blank ordinary words and are not keys anyway. Both a value and the forms
// html/template escapes it to are replaced. Nil when there is nothing to hide.
func secretReplacer(env []string) *strings.Replacer {
	seen := map[string]bool{}
	var values []string
	for _, kv := range env {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || len(value) < 12 || !secretName(name) {
			continue
		}
		for _, v := range []string{value, html.EscapeString(value), strings.ReplaceAll(html.EscapeString(value), "+", "&#43;")} {
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
