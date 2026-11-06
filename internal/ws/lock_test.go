package ws

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLockFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "merge.lock")
	release, busy, err := LockFile(path, "first merger")
	if err != nil || busy != "" {
		t.Fatalf("busy=%s err=%v", busy, err)
	}
	defer release()
	l, _, err := readLease(path)
	if err != nil || l.PID != os.Getpid() {
		t.Fatalf("lease=%+v err=%v", l, err)
	}
	if start, known := StartID(os.Getpid()); known && l.Start != start {
		t.Fatalf("start=%s want %s", l.Start, start)
	}
	if next, busy, err := LockFile(path, "second merger"); next != nil || err != nil || !strings.Contains(busy, "first merger") {
		t.Fatalf("busy=%s err=%v", busy, err)
	}
	release()
	if err := os.WriteFile(path, []byte(`{"holder":"dead","pid":2147483647}`), 0600); err != nil {
		t.Fatal(err)
	}
	next, busy, err := LockFile(path, "successor")
	if err != nil || busy != "" {
		t.Fatalf("busy=%s err=%v", busy, err)
	}
	release()
	if _, err := os.Stat(path); err != nil {
		t.Fatal("old release removed successor")
	}
	next()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("lock remains: %v", err)
	}
}
