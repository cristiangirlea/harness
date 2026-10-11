package tools

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// The secret files the guard keeps agents away from, in one list: the api
// backend's file tools refuse them (SecretFile), and claude's deny rules are
// built from the same names. The other CLIs take no such rules; their runs say
// so.

// SecretNames are file-name patterns, matched against a path's last element.
// .env* refuses a .env.example as well: a template costs the agent one turn,
// and .env.staging, .env.test and .envrc are what the pattern is for.
var SecretNames = []string{".env*", ".dev.vars", "*.pem", "*.key", ".git-credentials"}

// SecretDirs are directories whose contents are secret wherever they sit.
var SecretDirs = []string{".private", ".ssh", ".aws", ".kube", ".azure", ".wrangler"}

// SecretFile reports whether a slash- or backslash-separated path names a
// secret file or lies inside a secret directory. Case is ignored: on Windows
// .ENV is .env.
func SecretFile(path string) bool {
	parts := strings.FieldsFunc(strings.ToLower(path), func(r rune) bool { return r == '/' || r == '\\' })
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
	return false
}

// refuseSecretFile is the Refuse of the file tools that take a path: a
// secret file inside the workspace is refused unless the guard is lifted.
// The path is checked relative to the workspace, so a workspace that itself
// sits under a .private directory is not refused as a whole.
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
	if SecretFile(ws.Rel(p)) {
		return "secret-file"
	}
	return ""
}
