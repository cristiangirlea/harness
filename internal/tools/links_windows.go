//go:build windows

package tools

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// throughLink exists because since Go 1.23 EvalSymlinks stops at junctions and dangling links; ("", true) refuses.
func throughLink(p string) (string, bool) {
	return throughLinkWith(p, os.Lstat, os.Readlink)
}

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
			// Non-link reparse points are safe only as final components.
			if typ == fs.ModeIrregular && i == len(parts)-1 {
				continue
			}
			return "", true
		}
		switch {
		case filepath.IsAbs(target):
		case typ == fs.ModeIrregular, filepath.VolumeName(target) != "":
			// Relative mounts are unsafe; C:x depends on drive-local cwd.
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
