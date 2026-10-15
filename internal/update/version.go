// Package update moves every hrn binary on this computer to another build:
// it reads what each copy says with `version`, swaps the file under a backup,
// and restarts a running hrn watch. The command in cmd/hrn wires the real
// process list, network and file system; the pieces here take them as
// options so tests run with fakes.
package update

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Version is what one hrn binary printed for `version`, taken apart. The
// line is `hrn <release> (<go version>[, <module version>][, <commit>][,
// modified])`, as cmd/hrn/version.go writes it.
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

// ParseVersionLine reads the first line of a binary's `version` output. It
// fails for anything that is not hrn's line, which is how a target that is
// not hrn is refused.
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

// Same reports whether two version lines describe the same build: the same
// commit, or the same module version when neither names a commit, with the
// same edits. Two lines without any stamp compare whole.
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

// Short names the build for a backup file: the commit, else the module
// version, else the release, else "unknown".
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
