package batch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// hideMaxBytes bounds what one attempt keeps in memory; nothing is removed when the matches exceed it.
var hideMaxBytes int64 = 64 << 20

// hiddenFiles holds what was taken out of a worktree, and the handle the files go back through.
type hiddenFiles struct {
	// Opened before the agent ran, so a worktree path the agent swapped for a link cannot redirect the restore.
	root  *os.Root
	files []hiddenFile
}

type hiddenFile struct {
	name string // slash path
	data []byte
	perm fs.FileMode
}

func (h *hiddenFiles) count() int {
	if h == nil {
		return 0
	}
	return len(h.files)
}

// openPlainRoot refuses a link: an earlier attempt's agent may have swapped the worktree for one to another checkout.
func openPlainRoot(dir string) (*os.Root, error) {
	before, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is not a plain folder", dir)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	// The path may have been swapped between the two calls; the handle must be the folder that was checked.
	if opened, err := root.Stat("."); err != nil || !os.SameFile(before, opened) {
		root.Close()
		return nil, fmt.Errorf("%s changed while it was being opened", dir)
	}
	return root, nil
}

// hideFiles removes the regular files tracked at base that match, keeping their bytes as git checked them out.
// The caller restores whatever it returns, also when it returns an error.
func hideFiles(ctx context.Context, dir, base string, globs []string) (*hiddenFiles, error) {
	if len(globs) == 0 {
		return nil, nil
	}
	root, err := openPlainRoot(dir)
	if err != nil {
		return nil, err
	}
	h := &hiddenFiles{root: root}
	g, err := newAgentGit(ctx, dir)
	if err != nil {
		return h, err
	}
	defer g.close()
	out, err := g.stdout(ctx, "ls-tree", "-r", "-z", "--name-only", "--end-of-options", base)
	if err != nil {
		return h, err
	}
	// Everything is read, within the budget, before anything is removed.
	left := hideMaxBytes
	for name := range strings.SplitSeq(out, "\x00") {
		if name == "" || !slices.ContainsFunc(globs, func(g string) bool { return matchProtect(g, name) }) {
			continue
		}
		local := filepath.FromSlash(name)
		fi, err := root.Lstat(local)
		if errors.Is(err, fs.ErrNotExist) || err == nil && !fi.Mode().IsRegular() {
			continue // nothing the agent could read as a file, and nothing to give back
		}
		if err == nil {
			var data []byte
			if data, err = readWithin(root, local, left); err == nil {
				left -= int64(len(data))
				h.files = append(h.files, hiddenFile{name: name, data: data, perm: fi.Mode().Perm()})
				continue
			}
		}
		// Nothing was removed yet, so there is nothing to put back.
		return &hiddenFiles{root: root}, err
	}
	for i, f := range h.files {
		if err := root.Remove(filepath.FromSlash(f.name)); err != nil {
			// Only the removed ones go back; the rest were never touched.
			h.files = h.files[:i]
			return h, err
		}
	}
	return h, nil
}

// readWithin fails instead of reading past the budget, whatever size the file reported earlier.
func readWithin(root *os.Root, local string, budget int64) ([]byte, error) {
	f, err := root.Open(local)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, budget+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > budget {
		return nil, fmt.Errorf("the matching files hold more than the %d bytes that can be hidden", hideMaxBytes)
	}
	return data, nil
}

// restore writes the kept bytes back over whatever the agent left at those paths, and closes the handle.
func (h *hiddenFiles) restore() error {
	if h == nil {
		return nil
	}
	defer h.root.Close()
	var errs []error
	for _, f := range h.files {
		if err := h.write(f); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (h *hiddenFiles) write(f hiddenFile) error {
	if err := h.clear(f); err != nil {
		return err
	}
	return h.create(f)
}

// clear removes what the agent may have put where the file was: a file, a folder or a link.
func (h *hiddenFiles) clear(f hiddenFile) error {
	local := filepath.FromSlash(f.name)
	if err := h.root.RemoveAll(local); err != nil {
		return err
	}
	if parent := filepath.Dir(local); parent != "." {
		return h.root.MkdirAll(parent, 0o755)
	}
	return nil
}

// create is exclusive: a file that reappears after clear could be a hard link to one outside the worktree.
func (h *hiddenFiles) create(f hiddenFile) error {
	out, err := h.root.OpenFile(filepath.FromSlash(f.name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, f.perm)
	if err != nil {
		return err
	}
	if _, err := out.Write(f.data); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
