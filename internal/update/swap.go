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

// VersionFunc answers `<path> version` for one binary. RunVersion is the
// real one; tests run copies of the test binary.
type VersionFunc func(ctx context.Context, path string) (Version, error)

// versionTimeout bounds `version`: a binary that hangs there is not hrn.
const versionTimeout = 20 * time.Second

// RunVersion runs path with the single argument version and parses the
// first line. This is the only way a candidate or a target is ever run.
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

// ErrNoBackup says a rollback found no hrn-*.prev file beside the target.
var ErrNoBackup = errors.New("no backup beside it")

const stampLayout = "20060102-1504"

// BackupName is the file a target is renamed to before the candidate takes
// its place: hrn-<old build>-<yyyymmdd-hhmm>.prev, with the target's
// extension, in the same folder.
func BackupName(target string, old Version, now time.Time) string {
	return fmt.Sprintf("hrn-%s-%s.prev%s", old.Short(), now.Format(stampLayout), filepath.Ext(target))
}

func rolledBackName(target string, cur Version, now time.Time) string {
	return fmt.Sprintf("hrn-%s-%s.rolledback%s", cur.Short(), now.Format(stampLayout), filepath.Ext(target))
}

// Swap puts candidate in place of target: the target is renamed to its
// backup name beside it (a running binary can be renamed on Windows, not
// overwritten), the candidate is copied in under the original name with
// mode 0755, and the result is run with version and compared with want. On
// any failure the backup goes back and the error says what went wrong. The
// returned path is the backup.
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

// install copies src to dst with mode 0755, through a temporary file beside
// dst so a half-written binary never carries the real name.
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

// restore puts the backup back under the target's name after a failed swap
// and returns cause, or cause and the restore's own failure.
func restore(backup, target string, cause error) error {
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		// A new file that cannot be removed (it may be running) is renamed
		// out of the way instead; a .failed left by an earlier run gives way.
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

// Backups lists the hrn-*.prev files beside target with its extension,
// newest stamp first.
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

// Rollback puts the newest backup beside target back in its place: the
// current binary becomes hrn-<build>-<stamp>.rolledback, the backup takes
// the original name, and version must answer. On failure both renames are
// undone. It returns the version the target now reports.
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
