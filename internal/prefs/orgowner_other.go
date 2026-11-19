//go:build !windows

package prefs

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

func adminOrgOwner(uid uint32, mode fs.FileMode) bool {
	return uid == 0 && mode.Perm()&0022 == 0
}

func trustedOrgPath(path string) bool {
	for _, name := range []string{filepath.Dir(path), path} {
		info, err := os.Stat(name)
		if err != nil {
			return false
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !adminOrgOwner(stat.Uid, info.Mode()) {
			return false
		}
	}
	return true
}
