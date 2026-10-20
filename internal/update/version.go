// Package update replaces hrn binaries with backups and restarts watches.
package update

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Version holds parsed hrn release and build identity.
type Version struct {
	Line     string // the first line as printed
	Release  string // agent.Version, 0.2.0
	Module   string // the module version of a go install build, v0.1.0 or a pseudo-version
	Commit   string // the short commit of a checkout build, or the one a pseudo-version ends in
	Modified bool   // built from a tree with edits
}

var (
	pseudoRE = regexp.MustCompile(`-[0-9]{14}-([0-9a-f]{12})(\+dirty)?$`)
	commitRE = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
)

// ParseVersionLine rejects non-hrn output to protect unrelated binaries.
func ParseVersionLine(out string) (Version, error) {
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	line = strings.TrimSpace(line)
	rest, ok := strings.CutPrefix(line, "hrn ")
	if !ok {
		return Version{}, fmt.Errorf("%w: %q", ErrNotHrn, line)
	}
	release, parens, ok := strings.Cut(rest, " (")
	if !ok || !strings.HasSuffix(parens, ")") || release == "" || strings.ContainsAny(release, " \t") {
		return Version{}, fmt.Errorf("%w: %q", ErrNotHrn, line)
	}
	v := Version{Line: line, Release: release}
	parts := strings.Split(strings.TrimSuffix(parens, ")"), ", ")
	if len(parts) == 0 || !strings.HasPrefix(parts[0], "go") {
		return Version{}, fmt.Errorf("%w: %q", ErrNotHrn, line)
	}
	for _, p := range parts[1:] {
		switch {
		case p == "modified":
			v.Modified = true
		case strings.HasPrefix(p, "v") && len(p) > 1 && p[1] >= '0' && p[1] <= '9':
			v.Module = p
			if m := pseudoRE.FindStringSubmatch(p); m != nil {
				v.Commit = m[1]
				if m[2] != "" {
					v.Modified = true
				}
			}
		case commitRE.MatchString(p):
			v.Commit = p
		}
	}
	return v, nil
}

// Same compares build identity and edits, using full lines for unstamped builds.
func (v Version) Same(o Version) bool {
	if v.Modified != o.Modified {
		return false
	}
	switch {
	case v.Commit != "" && o.Commit != "":
		return v.Commit == o.Commit
	case v.Commit != "" || o.Commit != "":
		return false
	case v.Module != "" || o.Module != "":
		return v.Module == o.Module
	}
	return v.Line == o.Line
}

// Short selects commit, module, release or "unknown" for backups.
func (v Version) Short() string {
	switch {
	case v.Commit != "":
		return v.Commit
	case v.Module != "":
		return v.Module
	case v.Release != "":
		return v.Release
	}
	return "unknown"
}

// ErrNotHrn says a binary did not answer `version` the way hrn does.
var ErrNotHrn = errors.New("does not answer `version` like hrn")
