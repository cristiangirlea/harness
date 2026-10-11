package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcStatParserHandlesSpacesParenthesesAndZombies(t *testing.T) {
	for _, tc := range []struct {
		line  string
		pgid  int
		state byte
	}{
		{"123 (a command with spaces) S 1 456 789 0", 456, 'S'},
		{"123 (a command ) with ) parentheses)) R 1 456 789 0", 456, 'R'},
		{"123 (dead ) command) Z 1 456 789 0", 456, 'Z'},
	} {
		pgid, state, err := parseProcStat(tc.line)
		if err != nil || pgid != tc.pgid || state != tc.state {
			t.Errorf("parse %q = %d %c %v", tc.line, pgid, state, err)
		}
	}
	for _, line := range []string{"", "123 no parentheses S 1 456", "123 (missing close S 1 456", "123 (short) S 1", "123 (bad state) SS 1 456", "123 (bad group) S 1 nope"} {
		if _, _, err := parseProcStat(line); err == nil {
			t.Errorf("accepted malformed stat: %q", line)
		}
	}
}

func TestProcScanSkipsEntriesDeniedByPermissions(t *testing.T) {
	root := t.TempDir()
	for _, pid := range []string{"1", "2"} {
		if err := os.Mkdir(filepath.Join(root, pid), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, denied := range []error{syscall.EACCES, syscall.EPERM, os.ErrPermission} {
		t.Run(denied.Error(), func(t *testing.T) {
			read := func(path string) ([]byte, error) {
				if filepath.Base(filepath.Dir(path)) == "2" {
					return nil, &os.PathError{Op: "open", Path: path, Err: denied}
				}
				return []byte("1 (our command) S 0 42 0"), nil
			}
			n, err := countRunningGroupWith(root, 42, read)
			if err != nil || n != 1 {
				t.Fatalf("an unreadable unrelated entry must be skipped: n=%d err=%v", n, err)
			}
		})
	}
}

func TestWaitGoneFallsBackToTheProbeWhenTheScanFails(t *testing.T) {
	for _, scanErr := range []error{syscall.EACCES, errors.New("unavailable procfs"), errors.New("malformed stat")} {
		t.Run(scanErr.Error(), func(t *testing.T) {
			calls := 0
			started := time.Now()
			err := waitGroupGone(42, 30*time.Millisecond, func() error { return nil }, func() (int, error) {
				calls++
				return 0, scanErr
			})
			if err == nil || !strings.Contains(err.Error(), "at least 1 process still alive") || strings.Contains(err.Error(), "query live members") || calls < 2 || time.Since(started) < 30*time.Millisecond {
				t.Fatalf("an unavailable count must use bounded probe-based polling: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestWaitGoneKeepsPollingWhenProcFixturesAreUnreadable(t *testing.T) {
	root := t.TempDir()
	for _, pid := range []string{"1", "2"} {
		if err := os.Mkdir(filepath.Join(root, pid), 0700); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	read := func(path string) ([]byte, error) {
		if filepath.Base(filepath.Dir(path)) == "1" {
			// An unrelated user's listed entry is unreadable under hidepid=1.
			return nil, &os.PathError{Op: "open", Path: path, Err: syscall.EACCES}
		}
		// If the remaining scan also fails, no exact count is available.
		return nil, &os.PathError{Op: "read", Path: path, Err: syscall.EIO}
	}
	started := time.Now()
	err := waitGroupGone(42, 30*time.Millisecond, func() error { return nil }, func() (int, error) {
		calls++
		return countRunningGroupWith(root, 42, read)
	})
	if err == nil || !strings.Contains(err.Error(), "at least 1 process still alive") || strings.Contains(err.Error(), "query") || calls < 2 || time.Since(started) < 30*time.Millisecond {
		t.Fatalf("unreadable fixtures must keep polling with the probe's lower bound: calls=%d err=%v", calls, err)
	}
}

func TestWaitGoneBacksOffDuringLongWaits(t *testing.T) {
	calls := 0
	started := time.Now()
	err := waitGroupGone(42, 350*time.Millisecond, func() error { calls++; return nil }, func() (int, error) { return 1, nil })
	if err == nil || calls < 2 || calls > 12 || time.Since(started) < 350*time.Millisecond {
		t.Fatalf("want fast initial polls followed by fewer scans: calls=%d elapsed=%s err=%v", calls, time.Since(started), err)
	}
}

func TestProcScanCountsOnlyLiveMembersOfTheGroup(t *testing.T) {
	root := t.TempDir()
	write := func(pid int, state byte, pgid int) {
		dir := filepath.Join(root, fmt.Sprint(pid))
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		line := fmt.Sprintf("%d (command with ) spaces) %c 1 %d 0", pid, state, pgid)
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(line), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(1, 'Z', 42)
	write(2, 'S', 99)
	if n, err := countRunningGroup(root, 42); err != nil || n != 0 {
		t.Fatalf("zombies and other groups must not count: n=%d err=%v", n, err)
	}
	write(3, 'S', 42)
	write(4, 'R', 42)
	// A process may vanish between listing the directory and reading stat.
	if err := os.Mkdir(filepath.Join(root, "5"), 0700); err != nil {
		t.Fatal(err)
	}
	if n, err := countRunningGroup(root, 42); err != nil || n != 2 {
		t.Fatalf("want two live group members: n=%d err=%v", n, err)
	}
	if err := os.WriteFile(filepath.Join(root, "3", "stat"), []byte("malformed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := countRunningGroup(root, 42); err == nil {
		t.Fatal("a malformed stat must not prove the tree is gone")
	}
}

func TestWaitGoneIgnoresZombieOnlyGroups(t *testing.T) {
	err := waitGroupGone(42, 0, func() error { return nil }, func() (int, error) { return 0, nil })
	if err != nil {
		t.Fatalf("a group with no live members is gone: %v", err)
	}
}

func TestWaitGoneTreatsPermissionDeniedAsAlive(t *testing.T) {
	calls := 0
	started := time.Now()
	err := waitGroupGone(42, 30*time.Millisecond, func() error {
		calls++
		return syscall.EPERM
	}, func() (int, error) {
		t.Fatal("EPERM must not be overridden by a proc scan")
		return 0, nil
	})
	if err == nil || !strings.Contains(err.Error(), "still alive; at least one is owned by another user") || calls < 2 || time.Since(started) < 30*time.Millisecond {
		t.Fatalf("want bounded waiting and an ownership warning: calls=%d err=%v", calls, err)
	}
}

func TestWaitGoneReportsTheLiveProcessCount(t *testing.T) {
	err := waitGroupGone(42, 0, func() error { return nil }, func() (int, error) { return 3, nil })
	if err == nil || !strings.Contains(err.Error(), "3 processes still alive") {
		t.Fatalf("want the live process count: %v", err)
	}
}
