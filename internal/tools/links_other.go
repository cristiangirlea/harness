//go:build !windows

package tools

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// throughLink finds the first existing component of p that is a symbolic
// link filepath.EvalSymlinks left unresolved, and returns p with that
// component replaced by where it leads. EvalSymlinks fails on a link whose
// target does not exist yet, and realPath falls back to a prefix before the
// link, which would judge the link by its own name while a write follows it
// (#47). A mount is not a link here. The result is "" with true when the
// link cannot be read, or its target has a .. after a name: the system takes
// that .. after following the name, which may be a link too, so the target
// cannot be judged as text. A leading .. is taken from the link's own
// directory, which is real. realPath then refuses the path. A component that
// cannot be looked at (a missing or unsearchable directory) ends the walk:
// no link past it can be followed either.
func throughLink(p string) (string, bool) {
	cur := string(filepath.Separator)
	parts := strings.Split(strings.Trim(p, string(filepath.Separator)), string(filepath.Separator))
	for i, part := range parts {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			return "", false
		}
		if fi.Mode().Type() != fs.ModeSymlink {
			continue
		}
		target, err := os.Readlink(cur)
		if err != nil || dotDotAfterName(target) {
			return "", true
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(cur), target)
		}
		return filepath.Join(append([]string{target}, parts[i+1:]...)...), true
	}
	return "", false
}

// dotDotAfterName reports whether a link's target has a .. after a name.
func dotDotAfterName(target string) bool {
	named := false
	for _, e := range strings.Split(target, string(filepath.Separator)) {
		switch e {
		case "", ".":
		case "..":
			if named {
				return true
			}
		default:
			named = true
		}
	}
	return false
}
