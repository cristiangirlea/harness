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

// Cargo holds an exclusive flock while building; probe without waiting for it to finish.
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

func holdCargoLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
