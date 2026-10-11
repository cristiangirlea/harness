//go:build windows

package tools

import (
	"os"
	"path/filepath"
	"strings"
)

// throughMountPoint finds the first existing component of p that is a mount
// point (a directory junction, or a volume mounted on a folder) and returns p
// with that component replaced by where it leads. Since Go 1.23 (the
// winsymlink setting) Lstat reports a mount point as irregular rather than as
// a symbolic link, so filepath.EvalSymlinks walks through one as though it
// were a plain directory. Other reparse points are irregular too (OneDrive's
// files and folders, for one) but are not links: os.Readlink refuses them and
// they are passed over.
func throughMountPoint(p string) (string, bool) {
	vol := filepath.VolumeName(p)
	cur := vol + string(filepath.Separator)
	parts := strings.Split(strings.Trim(p[len(vol):], `\`), `\`)
	for i, part := range parts {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			return "", false
		}
		if fi.Mode().Type() != os.ModeIrregular {
			continue
		}
		target, err := os.Readlink(cur)
		if err != nil {
			continue
		}
		return filepath.Join(append([]string{target}, parts[i+1:]...)...), true
	}
	return "", false
}
