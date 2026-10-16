//go:build windows

package followup

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
)

// shortName is long's 8.3 name. The test skips on a volume that makes none.
func shortName(t *testing.T, long string) string {
	t.Helper()
	p, err := syscall.UTF16PtrFromString(long)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, 1024)
	n, err := syscall.GetShortPathName(p, &buf[0], uint32(len(buf)))
	if err != nil || n == 0 || int(n) > len(buf) {
		t.Skipf("no short name for %s: %v", long, err)
	}
	short := syscall.UTF16ToString(buf[:n])
	if strings.EqualFold(filepath.Base(short), filepath.Base(long)) {
		t.Skipf("this volume makes no 8.3 names (%s)", short)
	}
	return short
}

func TestASessionRecordedUnderAShortNameIsNotForkedFromItsLongName(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	long := filepath.Join(workspace(t), "session workspace")
	if err := os.Mkdir(long, 0o755); err != nil {
		t.Fatal(err)
	}
	short := shortName(t, long)
	resolved, err := filepath.EvalSymlinks(short)
	if err != nil || resolved != long {
		t.Fatalf("short name resolves to %q, want %q: %v", resolved, long, err)
	}
	id := finished(t, "", agent.Task{Prompt: "p", Dir: short}, agent.Result{Backend: "claude", Session: "s1"})
	_, err = Make(Request{RunID: id, Dir: long})
	if err == nil || !strings.Contains(err.Error(), "its session belongs to "+short) {
		t.Fatalf("expected refusal naming the session's short directory, got %v", err)
	}
}
