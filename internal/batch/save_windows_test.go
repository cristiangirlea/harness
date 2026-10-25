package batch

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestASaveOutlastsWindowsRenameLocks(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  syscall.Errno
	}{
		{"sharing violation", syscall.Errno(32)},
		{"lock violation", syscall.Errno(33)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			res := Result{ID: "brief-lock", Started: time.Unix(1, 0)}
			if err := SaveResult(res); err != nil {
				t.Fatal(err)
			}
			rename, sleep := saveRename, saveSleep
			t.Cleanup(func() { saveRename, saveSleep = rename, sleep })
			var waited time.Duration
			saveSleep = func(d time.Duration) { waited += d }
			saveRename = func(root *os.Root, old, new string) error {
				if waited < 350*time.Millisecond {
					return &os.LinkError{Op: "rename", Old: old, New: new, Err: tc.err}
				}
				return rename(root, old, new)
			}
			res.Started = time.Unix(2, 0)
			if err := SaveResult(res); err != nil {
				t.Fatalf("save after %v of retries: %v", waited, err)
			}
			saved, err := LoadResult(res.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !saved.Started.Equal(res.Started) {
				t.Errorf("saved start = %v, want %v", saved.Started, res.Started)
			}
			if _, err := os.Stat(filepath.Join(batchesDir(), res.ID+".json.tmp")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("temporary file stat = %v, want not exist", err)
			}
		})
	}
}
