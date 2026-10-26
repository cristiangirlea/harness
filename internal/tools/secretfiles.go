package tools

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"unicode"
)

// SecretNames shares API/Claude rules; .env* sacrifices templates to protect variants.
var SecretNames = []string{
	".env*", ".dev.vars",
	"*.pem", "*.key", "*.p12", "*.pfx", "*.jks", "*.keystore",
	".git-credentials", ".netrc", "_netrc", ".npmrc", ".pypirc",
}

// SecretDirs protects whole directories anywhere.
var SecretDirs = []string{".private", ".ssh", ".aws", ".kube", ".azure"}

// SecretPaths lists case-insensitive logins; trailing slash protects subtrees.
// Disk case serves Claude; only .wrangler/config holds secrets.
var SecretPaths = []string{
	".claude/.credentials.json", ".codex/auth.json", ".gemini/oauth_creds.json", ".local/share/opencode/auth.json",
	".config/gh/", "GitHub CLI/", ".config/gcloud/", ".docker/config.json", ".cargo/credentials*",
	".wrangler/config/",
}

// SecretFile handles Windows case/slash/dot/space/stream aliases.
// Callers must expand 8.3 names first.
func SecretFile(path string) bool {
	var parts []string
	for _, part := range strings.FieldsFunc(strings.ToLower(path), func(r rune) bool { return r == '/' || r == '\\' }) {
		if c := strings.IndexByte(part, ':'); c > 0 {
			part = part[:c]
		}
		if part = strings.TrimRight(part, ". "); part != "" { // . and .. go
			parts = append(parts, part)
		}
	}
	for i, part := range parts {
		for _, dir := range SecretDirs {
			if part == dir {
				return true
			}
		}
		if i < len(parts)-1 {
			continue
		}
		for _, name := range SecretNames {
			if ok, _ := filepath.Match(name, part); ok {
				return true
			}
		}
	}
	for _, p := range SecretPaths {
		if underPath(parts, p) {
			return true
		}
	}
	return false
}

func underPath(parts []string, secret string) bool {
	dir := strings.HasSuffix(secret, "/")
	want := strings.Split(strings.ToLower(strings.TrimSuffix(secret, "/")), "/")
	for i := 0; i+len(want) <= len(parts); i++ {
		if !dir && i+len(want) != len(parts) {
			continue
		}
		match := true
		for j, w := range want {
			if ok, _ := filepath.Match(w, parts[i+j]); !ok {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// Textual checks miss implicit reads such as recursive grep.
func namesSecretFile(command string) bool {
	words := strings.FieldsFunc(command, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("<>|;&()\"'`=,:{}", r)
	})
	for _, w := range words {
		if SecretFile(w) {
			return true
		}
	}
	// Quoted paths retain spaces, e.g. GitHub CLI/hosts.yml.
	for _, quote := range []string{`"`, `'`} {
		segs := strings.Split(command, quote)
		for i := 1; i < len(segs); i += 2 {
			if SecretFile(segs[i]) {
				return true
			}
		}
	}
	return false
}

// Relative checks spare ancestors; resolve to catch secret aliases.
func refuseSecretFile(ws *Workspace, raw json.RawMessage) string {
	var args struct {
		Path string `json:"path"`
	}
	if ws.AllowDangerous || json.Unmarshal(raw, &args) != nil {
		return "" // a call that cannot be read is the loop's to report
	}
	p, err := ws.Resolve(args.Path)
	if err != nil {
		return "" // outside the workspace: Run says so
	}
	if ws.secretPath(p) {
		return "secret-file"
	}
	return ""
}
