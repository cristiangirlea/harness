package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Tests need to replace paths between resolution and opening.
var beforeOpenInside func(real string)

func (w *Workspace) openInside(p string, flag int, perm os.FileMode) (f *os.File, created bool, err error) {
	real := realPath(p)
	if real == "" || w.real == "" {
		return nil, false, fmt.Errorf("path %q is refused: hrn cannot tell where it leads (a link that loops or cannot be read, or a name the system refuses)", p)
	}
	if !w.inside(real) {
		return nil, false, fmt.Errorf("path %q is outside the workspace %s", p, w.Root)
	}
	rel, err := filepath.Rel(w.real, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, false, fmt.Errorf("path %q is outside the workspace %s", p, w.Root)
	}
	// A link swapped in after the tool's own check can lead to a secret file inside the workspace.
	if !w.AllowDangerous && w.secretReal(real) {
		return nil, false, GuardError("secret-file")
	}
	checked, _ := os.Lstat(real)
	if beforeOpenInside != nil {
		beforeOpenInside(real)
	}
	root, err := os.OpenRoot(w.real)
	if err != nil {
		return nil, false, err
	}
	defer root.Close()
	flag = flag&^(os.O_CREATE|os.O_TRUNC) | readOpenFlags
	if flag&(os.O_WRONLY|os.O_RDWR) != 0 {
		if dir := filepath.Dir(rel); dir != "." {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				return nil, false, err
			}
		}
		f, err = root.OpenFile(rel, flag|os.O_CREATE|os.O_EXCL, perm)
		created = err == nil
		if errors.Is(err, os.ErrExist) {
			f, err = root.OpenFile(rel, flag, perm)
		}
	} else {
		f, err = root.OpenFile(rel, flag, 0)
	}
	if err != nil {
		return nil, false, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, false, fmt.Errorf("%q is not a regular file", w.Rel(p))
	}
	// os.Root follows links that stay inside it, so the file opened must still be the one checked.
	if checked != nil && !os.SameFile(info, checked) || !w.stillResolves(rel, real, info) {
		f.Close()
		return nil, false, fmt.Errorf("path %q is refused: it changed while hrn was opening it", p)
	}
	return f, created, nil
}

// stillResolves reports whether rel still names the opened file through plain directories, as realPath left it.
func (w *Workspace) stillResolves(rel, real string, opened os.FileInfo) bool {
	dir := w.real
	if parent := filepath.Dir(rel); parent != "." {
		for _, part := range strings.Split(parent, string(filepath.Separator)) {
			dir = filepath.Join(dir, part)
			if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
				return false
			}
		}
	}
	now, err := os.Lstat(real)
	return err == nil && os.SameFile(opened, now)
}
