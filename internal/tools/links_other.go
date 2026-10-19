//go:build !windows

package tools

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Writes follow dangling links EvalSymlinks misses; ("", true) refuses.
func throughLink(p string) (string, bool) {
	cur := string(filepath.Separator)
	parts := strings.Split(strings.Trim(p, string(filepath.Separator)), string(filepath.Separator))
	for i, part := range parts {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return "", false
		}
		// Other errors can hide reachable paths: fail closed.
		if err != nil {
			return "", true
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

// name/.. may traverse a link; only leading .. is safe to clean.
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
