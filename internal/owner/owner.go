// Package owner names who a run or batch belongs to and decides who may read it.
package owner

import (
	"os"
	"os/user"
	"strings"
	"unicode"
)

// LocalPrefix marks the user of a local install; email: and user: are for shared installs.
const LocalPrefix = "local:"

const maxLen = 200

// Local is this install's user: $HRN_OWNER when valid, else local:<OS user name>.
func Local() string {
	if o := strings.TrimSpace(os.Getenv("HRN_OWNER")); Valid(o) {
		return o
	}
	name := ""
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	if name == "" {
		name = os.Getenv("USER") + os.Getenv("USERNAME")
	}
	// DOMAIN\name on Windows; the domain adds nothing locally.
	if i := strings.LastIndexAny(name, `\/`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.ToLower(strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("._-", r) {
			return r
		}
		return -1
	}, name))
	if name == "" {
		name = "user"
	}
	return LocalPrefix + name
}

// Valid reports whether id is kind:value with a known kind and no control characters.
func Valid(id string) bool {
	if id == "" || len(id) > maxLen || strings.ContainsFunc(id, unicode.IsControl) {
		return false
	}
	kind, value, ok := strings.Cut(id, ":")
	if !ok || strings.TrimSpace(value) == "" {
		return false
	}
	switch kind {
	case "local", "email", "user":
		return true
	}
	return false
}

// Of is a record's effective owner: records written before ownership belong to the local user.
func Of(recorded string) string {
	if recorded == "" {
		return Local()
	}
	return recorded
}

// Email is the principal of a verified email address.
func Email(addr string) string { return "email:" + strings.ToLower(strings.TrimSpace(addr)) }

// Viewer is who is asking; Admin reads every record and is a separate grant from ownership.
type Viewer struct {
	ID     string
	Admin  bool
	Grants []Grant
	OnRead func(id, basis string)
}

// LocalViewer is the person at this install.
func LocalViewer() Viewer { return Viewer{ID: Local()} }

// CanRead keeps ownership checks available when no directory is known.
func (v Viewer) CanRead(recorded string) bool {
	return v.CanReadRecord(recorded, "")
}
