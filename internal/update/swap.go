package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// VersionFunc reads a binary's version.
type VersionFunc func(ctx context.Context, path string) (Version, error)

const versionTimeout = 20 * time.Second

// RunVersion executes only the version command and parses its first line.
func RunVersion(ctx context.Context, path string) (Version, error) {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version")
	cmd.Stdin = nil
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(out) == 0 {
			return Version{}, fmt.Errorf("%w: %s version: %v", ErrNotHrn, path, err)
		} else if !errors.As(err, &exit) {
			return Version{}, fmt.Errorf("%s version: %w", path, err)
		}
	}
	v, err := ParseVersionLine(string(out))
	if err != nil {
		return Version{}, fmt.Errorf("%s: %w", path, err)
	}
	return v, nil
}

// ErrNoBackup indicates no adjacent rollback backup.
var ErrNoBackup = errors.New("no backup beside it")

const stampLayout = "20060102-1504"

// BackupName returns hrn-<build>-<stamp>.prev with target's extension.
func BackupName(target string, old Version, now time.Time) string {
	return fmt.Sprintf("hrn-%s-%s.prev%s", old.Short(), now.Format(stampLayout), filepath.Ext(target))
}

func rolledBackName(target string, cur Version, now time.Time) string {
	return fmt.Sprintf("hrn-%s-%s.rolledback%s", cur.Short(), now.Format(stampLayout), filepath.Ext(target))
}

// Swap installs and verifies candidate, restoring on failure and returning its backup path on success.
// Windows allows renaming running binaries, but forbids overwriting them.
func Swap(ctx context.Context, target, candidate string, old, want Version, now time.Time, version VersionFunc) (string, error) {
	if _, err := os.Stat(target); err != nil {
		return "", err
	}
	backup := filepath.Join(filepath.Dir(target), BackupName(target, old, now))
	if _, err := os.Stat(backup); err == nil {
		return "", fmt.Errorf("%s exists already; wait a minute or move it", backup)
	}
	if err := os.Rename(target, backup); err != nil {
		return "", fmt.Errorf("rename aside: %w", err)
	}
	if err := install(candidate, target); err != nil {
		return "", restore(backup, target, fmt.Errorf("copy in: %w", err))
	}
	got, err := version(ctx, target)
	if err != nil {
		return "", restore(backup, target, fmt.Errorf("the new file does not answer version: %w", err))
	}
	if got.Line != want.Line {
		return "", restore(backup, target, fmt.Errorf("the new file says %q, the candidate said %q", got.Line, want.Line))
	}
	return backup, nil
}

// Rename an adjacent complete copy to avoid exposing a partial executable.
func install(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".hrn-update-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

func restore(backup, target string, cause error) error {
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		// Windows may allow renaming a running binary when deletion fails.
		os.Remove(target + ".failed")
		if err := os.Rename(target, target+".failed"); err != nil {
			return fmt.Errorf("%w; and the failed file could not be moved: %v; the old binary is %s", cause, err, backup)
		}
	}
	if err := os.Rename(backup, target); err != nil {
		return fmt.Errorf("%w; and the backup could not be put back: %v; the old binary is %s", cause, err, backup)
	}
	return fmt.Errorf("%w; the old binary was put back", cause)
}

var backupRE = regexp.MustCompile(`^hrn-(.+)-([0-9]{8}-[0-9]{4})\.prev$`)

// Backups lists adjacent hrn-*.prev files by extension, newest first.
func Backups(target string) ([]string, error) {
	dir, ext := filepath.Dir(target), filepath.Ext(target)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	type backup struct {
		path, stamp string
	}
	var found []backup
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if ext != "" {
			if !strings.EqualFold(filepath.Ext(name), ext) {
				continue
			}
			name = name[:len(name)-len(ext)]
		}
		m := backupRE.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		found = append(found, backup{filepath.Join(dir, e.Name()), m[2]})
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].stamp > found[j].stamp })
	out := make([]string, len(found))
	for i, b := range found {
		out[i] = b.path
	}
	return out, nil
}

// Rollback restores the newest backup and returns its version, undoing renames on verification failure.
func Rollback(ctx context.Context, target string, now time.Time, version VersionFunc) (Version, error) {
	backups, err := Backups(target)
	if err != nil {
		return Version{}, err
	}
	if len(backups) == 0 {
		return Version{}, fmt.Errorf("%s: %w", target, ErrNoBackup)
	}
	backup := backups[0]
	cur, _ := version(ctx, target)
	aside := filepath.Join(filepath.Dir(target), rolledBackName(target, cur, now))
	if _, err := os.Stat(aside); err == nil {
		return Version{}, fmt.Errorf("%s exists already; wait a minute or move it", aside)
	}
	if err := os.Rename(target, aside); err != nil {
		return Version{}, fmt.Errorf("rename aside: %w", err)
	}
	if err := os.Rename(backup, target); err != nil {
		os.Rename(aside, target)
		return Version{}, fmt.Errorf("put the backup in place: %w", err)
	}
	got, err := version(ctx, target)
	if err != nil {
		undo := fmt.Errorf("%s does not answer version: %w", filepath.Base(backup), err)
		if err := os.Rename(target, backup); err != nil {
			return Version{}, fmt.Errorf("%w; and it could not be moved back: %v", undo, err)
		}
		if err := os.Rename(aside, target); err != nil {
			return Version{}, fmt.Errorf("%w; and the current binary could not be put back from %s: %v", undo, aside, err)
		}
		return Version{}, fmt.Errorf("%w; nothing changed", undo)
	}
	return got, nil
}
