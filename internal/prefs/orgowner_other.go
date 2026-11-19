//go:build !windows

package prefs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

func adminOrgOwner(uid uint32, mode fs.FileMode) bool {
	return uid == 0 && mode.Perm()&0022 == 0
}

func openTrustedOrg(path string) (*os.File, error) {
	skipOwner := path == orgPathOverride && orgPathTrusted
	info, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return nil, orgOpenError(err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errOrgOwner
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, errOrgRead
	}
	if !skipOwner && !adminOrgOwner(uint32(stat.Uid), info.Mode()) {
		return nil, errOrgOwner
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, syscall.ELOOP) {
		return nil, errOrgOwner
	}
	if err != nil {
		return nil, orgOpenError(err)
	}
	if err := checkOrgFile(f, skipOwner); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func checkOrgFile(f *os.File, skipOwner bool) error {
	info, err := f.Stat()
	if err != nil {
		return errOrgRead
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errOrgRead
	}
	if !info.Mode().IsRegular() || uint64(stat.Nlink) != 1 || !skipOwner && !adminOrgOwner(uint32(stat.Uid), info.Mode()) {
		return errOrgOwner
	}
	return nil
}
