package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestConversationPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file modes are not enforced on Windows")
	}
	dir := t.TempDir()
	res, _, _ := runFake(t, [][]string{textTurn("done")}, Task{Prompt: "go", Dir: t.TempDir(), RunDir: dir})
	if res.Status != StatusOK {
		t.Fatalf("run: %+v", res)
	}
	info, err := os.Stat(filepath.Join(dir, "messages.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("messages.json mode = %04o, want 0600", got)
	}
}
