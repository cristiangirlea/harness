package tools

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"unicode"
)

// The secret files the guard keeps agents away from, in one list: the api
// backend's file tools and run_command refuse them (SecretFile,
// namesSecretFile), and claude's deny rules are built from the same names.
// The other CLIs take no such rules; their runs say so.

// SecretNames are file-name patterns, matched against a path's last element:
// environment files, keys and certificates, and the files curl, git, pip and
// twine keep logins in. .env* refuses a .env.example as well: a template costs
// the agent one turn, and .env.staging, .env.test and .envrc are what the
// pattern is for.
var SecretNames = []string{
	".env*", ".dev.vars",
	"*.pem", "*.key", "*.p12", "*.pfx", "*.jks", "*.keystore",
	".git-credentials", ".netrc", "_netrc", ".pypirc",
}

// SecretDirs are directories whose contents are secret wherever they sit.
var SecretDirs = []string{".private", ".ssh", ".aws", ".kube", ".azure", ".wrangler"}

// SecretFile reports whether a slash- or backslash-separated path names a
// secret file or lies inside a secret directory. Case is ignored, and so are
// the other spellings Windows takes for the same file: trailing dots and
// spaces (.env. is .env) and a stream name (.env::$DATA). Short 8.3 names are
// the caller's to expand (realPath does).
func SecretFile(path string) bool {
	parts := strings.FieldsFunc(strings.ToLower(path), func(r rune) bool { return r == '/' || r == '\\' })
	for i, part := range parts {
		if c := strings.IndexByte(part, ':'); c > 0 {
			part = part[:c]
		}
		if part = strings.TrimRight(part, ". "); part == "" {
			continue // . and ..
		}
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
	return false
}

// namesSecretFile reports whether a command line names a secret file: as an
// argument, a redirect's target or a flag's value. It reads words, not what
// the command does, so `echo "*.pem" >> .gitignore` is refused as well; a
// recursive grep or a tool that opens .env by itself is not seen.
func namesSecretFile(command string) bool {
	words := strings.FieldsFunc(command, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("<>|;&()\"'`=,", r)
	})
	for _, w := range words {
		if SecretFile(w) {
			return true
		}
	}
	return false
}

// refuseSecretFile is the Refuse of the file tools that take a path: a
// secret file inside the workspace is refused unless the guard is lifted.
// The path is checked relative to the workspace, so a workspace that itself
// sits under a .private directory is not refused as a whole, and both as
// written and where its links lead, so notes.txt -> .env is refused too.
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
