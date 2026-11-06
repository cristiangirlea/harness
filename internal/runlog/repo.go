package runlog

import (
	"net/url"
	"strings"
	"unicode/utf8"
)

// RepoName names a remote URL's repository as host/path; user names, tokens, ports and local paths never come through.
func RepoName(remote string) string {
	remote = strings.TrimSpace(remote)
	var host, path string
	if scheme, rest, ok := strings.Cut(remote, "://"); ok {
		switch scheme = strings.ToLower(scheme); scheme {
		case "https", "http", "ssh", "git", "git+ssh", "ssh+git":
		default:
			return ""
		}
		u, err := url.Parse(scheme + "://" + rest)
		if err != nil {
			return ""
		}
		host, path = u.Hostname(), u.Path
	} else {
		before, after, ok := strings.Cut(remote, ":")
		// A colon after a slash, or after one letter (a drive), is a local path.
		if !ok || strings.ContainsAny(before, `/\`) || len(before) == 1 {
			return ""
		}
		if at := strings.LastIndex(before, "@"); at >= 0 {
			before = before[at+1:]
		}
		host, path = before, after
	}
	host = strings.ToLower(host)
	name := host + "/" + strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if !ValidRepo(name) {
		return ""
	}
	return name
}

// ValidRepo reports whether name is a host and two to eight plain path segments, as RepoName gives.
func ValidRepo(name string) bool {
	host, path, _ := strings.Cut(name, "/")
	parts := strings.Split(path, "/")
	if len(name) > 200 || !validHost(host) || len(parts) < 2 || len(parts) > 8 {
		return false
	}
	for _, p := range parts {
		if !validSegment(p) {
			return false
		}
	}
	return true
}

func validHost(h string) bool {
	if h == "" || len(h) > 253 || h[0] == '.' || h[0] == '-' {
		return false
	}
	for _, c := range h {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '-') {
			return false
		}
	}
	return true
}

func validSegment(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 100 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// PickRemote is origin's URL, or the only remote's when there is no origin.
func PickRemote(remotes map[string]string) string {
	if u, ok := remotes["origin"]; ok {
		return u
	}
	if len(remotes) == 1 {
		for _, u := range remotes {
			return u
		}
	}
	return ""
}

// RemotesInConfig reads each remote's first url from a git config file's text.
func RemotesInConfig(data string) map[string]string {
	remotes := map[string]string{}
	name, in := "", false
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			name, in = remoteSection(line)
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !in || !ok || !strings.EqualFold(strings.TrimSpace(key), "url") {
			continue
		}
		if _, seen := remotes[name]; !seen {
			remotes[name] = configValue(value)
		}
	}
	return remotes
}

// remoteSection reads a [remote "name"] header, comments and tabs allowed as git allows them.
func remoteSection(line string) (string, bool) {
	kind, rest, ok := strings.Cut(strings.TrimPrefix(line, "["), `"`)
	if !ok || !strings.EqualFold(strings.TrimSpace(kind), "remote") {
		return "", false
	}
	var b strings.Builder
	for i := 0; i < len(rest); i++ {
		switch c := rest[i]; {
		case c == '\\' && i+1 < len(rest):
			i++
			b.WriteByte(rest[i])
		case c == '"':
			return b.String(), strings.HasPrefix(strings.TrimSpace(rest[i+1:]), "]")
		default:
			b.WriteByte(c)
		}
	}
	return "", false
}

// configValue reads a value as git does: quotes joined, escapes undone, an unquoted # or ; starting a comment.
func configValue(v string) string {
	var b strings.Builder
	quoted := false
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c == '\\' && i+1 < len(v):
			i++
			switch e := v[i]; e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'b':
				b.WriteByte('\b')
			default:
				b.WriteByte(e)
			}
		case c == '"':
			quoted = !quoted
		case !quoted && (c == '#' || c == ';'):
			return strings.TrimSpace(b.String())
		default:
			b.WriteByte(c)
		}
	}
	return strings.TrimSpace(b.String())
}

// startRepo reads the repository and branch a run starts on; a detached HEAD has no branch.
func startRepo(root string) (repo, branch string) {
	remotes := map[string]string{}
	out, _ := gitOutput(root, "config", "--local", "--get-regexp", `^remote\..*\.url$`)
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		name, isURL := strings.CutSuffix(strings.TrimPrefix(key, "remote."), ".url")
		if !ok || !isURL {
			continue
		}
		if _, seen := remotes[name]; !seen {
			remotes[name] = value
		}
	}
	repo = RepoName(PickRemote(remotes))
	if out, err := gitOutput(root, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		branch = strings.TrimSpace(out)
	}
	if len(branch) > 255 || !utf8.ValidString(branch) || strings.ContainsFunc(branch, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		branch = ""
	}
	return repo, branch
}
