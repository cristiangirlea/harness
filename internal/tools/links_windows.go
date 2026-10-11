//go:build windows

package tools

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// throughLink finds the first existing component of p that is a link
// filepath.EvalSymlinks left unresolved, and returns p with that component
// replaced by where it leads. Since Go 1.23 (the winsymlink setting) Lstat
// reports a mount point (a directory junction, or a volume mounted on a
// folder) as irregular rather than as a link, so EvalSymlinks does not follow
// one and fails on a path that goes on past it. A symbolic link can be left
// too: EvalSymlinks fails when its target does not exist yet or goes on past
// a junction, and realPath falls back to a prefix before the link. Other
// reparse points are irregular as well (OneDrive's files and folders, for
// one) but are not links: os.Readlink refuses them and they are passed over,
// unless the path goes on past one. The result is "" with true when a
// component cannot be judged, and realPath then refuses the path.
func throughLink(p string) (string, bool) {
	return throughLinkWith(p, os.Lstat, os.Readlink)
}

// throughLinkWith is throughLink on the given file system calls, so a test
// can show it links that only a privilege could make here.
func throughLinkWith(p string, lstat func(string) (fs.FileInfo, error), readlink func(string) (string, error)) (string, bool) {
	vol := filepath.VolumeName(p)
	cur := vol + string(filepath.Separator)
	parts := strings.Split(strings.Trim(p[len(vol):], `\`), `\`)
	for i, part := range parts {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return "", false
		}
		if err != nil {
			return "", true
		}
		typ := fi.Mode().Type()
		if typ != fs.ModeIrregular && typ != fs.ModeSymlink {
			continue
		}
		target, err := readlink(cur)
		if err != nil {
			if typ == fs.ModeIrregular && i == len(parts)-1 {
				continue
			}
			return "", true
		}
		switch {
		case filepath.IsAbs(target):
		case typ == fs.ModeIrregular, filepath.VolumeName(target) != "":
			// A mount point's target is absolute unless made by hand, and
			// C:x is relative to that drive's working directory.
			return "", true
		case strings.HasPrefix(target, `\`):
			target = vol + target // rooted: on the link's own volume
		default:
			target = filepath.Join(filepath.Dir(cur), target)
		}
		return filepath.Join(append([]string{target}, parts[i+1:]...)...), true
	}
	return "", false
}
