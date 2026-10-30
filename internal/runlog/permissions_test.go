package runlog

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
)

func TestNewRunPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file modes are not enforced on Windows")
	}
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := Create("stub", agent.Task{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{}); err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]os.FileMode{
		".": 0o700, "task.json": 0o600, "events.jsonl": 0o600, "result.json": 0o600,
	} {
		info, err := os.Stat(filepath.Join(w.Dir(), name))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != mode {
			t.Errorf("%s mode = %04o, want %04o", name, got, mode)
		}
	}
}
