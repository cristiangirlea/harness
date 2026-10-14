//go:build unix

package gc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// diskFree asks the file system holding path how much it has free for this
// user, and names the volume by its device number and mount point.
func diskFree(path string) (Drive, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Drive{}, &fs.PathError{Op: "statfs", Path: path, Err: err}
	}
	mount, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Drive{}, err
	}
	mount, err = filepath.Abs(mount)
	if err != nil {
		return Drive{}, err
	}
	info, err := os.Stat(mount)
	if err != nil {
		return Drive{}, err
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return Drive{}, errors.New("no device number for " + mount)
	}
	dev := sys.Dev
	for {
		parent := filepath.Dir(mount)
		if parent == mount {
			break
		}
		info, err := os.Stat(parent)
		if err != nil {
			return Drive{}, err
		}
		parentSys, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return Drive{}, errors.New("no device number for " + parent)
		}
		if parentSys.Dev != dev {
			break
		}
		mount = parent
	}
	return Drive{ID: fmt.Sprintf("device %d", dev), Name: mount, Free: int64(st.Bavail) * int64(st.Bsize)}, nil
}

// lockHeld reports whether a build holds the lock file. Cargo takes an
// exclusive flock on .cargo-lock while it builds, so a non-blocking one fails
// while one runs. A missing file is held by nobody.
func lockHeld(path string) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return false, nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		return true, nil
	}
	return false, &fs.PathError{Op: "flock", Path: path, Err: err}
}
