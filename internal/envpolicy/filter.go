package envpolicy

import "strings"

func Filter(env []string) []string {
	out := make([]string, 0, len(env))
	for _, v := range env {
		key, _, _ := strings.Cut(v, "=")
		key = strings.ToUpper(key)
		if key == "HRN_DATABASE_URL" || (strings.HasPrefix(key, "HRN_") && strings.HasSuffix(key, "_TOKEN")) {
			continue
		}
		out = append(out, v)
	}
	return out
}
