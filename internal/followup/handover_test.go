package followup

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHandoverReadsARecordedBaseOnlyAsARevision(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	if !supportsNoLazyFetch() {
		t.Skip("git does not support the handover safeguards")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "--allow-empty", "-m", "base"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	written := filepath.Join(t.TempDir(), "written")
	base := "--output=" + written
	writeBatch(t, "recorded", fmt.Sprintf(`{"id":"recorded","base":%q,"tasks":[{"id":"task","attempts":[{"run_id":"run"}]}]}`, base))
	attempt, unreadable := findAttempt("run")
	if unreadable != 0 || attempt.base != base {
		t.Fatalf("attempt = %+v, unreadable = %d", attempt, unreadable)
	}
	if got := changes(dir, attempt.base); !strings.Contains(got, "git diff failed:") {
		t.Errorf("changes did not refuse the recorded base:\n%s", got)
	}
	if _, err := os.Stat(written); !os.IsNotExist(err) {
		t.Errorf("git diff wrote %s (%v)", written, err)
	}
}
