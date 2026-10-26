package batch

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestASaveOutlastsABriefRenameLock(t *testing.T) {
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
			return &os.LinkError{Op: "renameat", Old: old, New: new, Err: os.ErrPermission}
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
}

func TestASaveThatNeverRenamesGivesUpAndRemovesItsTemp(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	res := Result{ID: "permanent-lock", Started: time.Unix(1, 0)}
	if err := SaveResult(res); err != nil {
		t.Fatal(err)
	}
	rename, sleep := saveRename, saveSleep
	t.Cleanup(func() { saveRename, saveSleep = rename, sleep })
	var waited, previous time.Duration
	grew := false
	saveSleep = func(d time.Duration) {
		if d <= 0 || d > 200*time.Millisecond || d < previous && waited+d != 2*time.Second {
			t.Errorf("retry sleep = %v after %v, previous %v", d, waited, previous)
		}
		if waited == 0 && d != 10*time.Millisecond {
			t.Errorf("first retry sleep = %v, want 10ms", d)
		}
		grew = grew || previous > 0 && d > previous
		waited += d
		if waited > 2*time.Second {
			t.Fatalf("retry budget exceeded: %v", waited)
		}
		previous = d
	}
	locked := &os.LinkError{Op: "renameat", Old: res.ID + ".json.tmp", New: res.ID + ".json", Err: os.ErrPermission}
	saveRename = func(*os.Root, string, string) error { return locked }
	res.Started = time.Unix(2, 0)
	if err := SaveResult(res); err != locked {
		t.Errorf("save error = %v, want %v", err, locked)
	}
	if waited != 2*time.Second || !grew {
		t.Errorf("retry budget = %v, grew = %v, want 2s with growing sleeps", waited, grew)
	}
	if _, err := os.Stat(filepath.Join(batchesDir(), res.ID+".json.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temporary file stat = %v, want not exist", err)
	}
	saved, err := LoadResult(res.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Started.Equal(time.Unix(1, 0)) {
		t.Errorf("saved start = %v, want the previous record preserved", saved.Started)
	}
}

func TestASaveReturnsOtherRenameErrorsWithoutRetrying(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	rename, sleep := saveRename, saveSleep
	t.Cleanup(func() { saveRename, saveSleep = rename, sleep })
	calls := 0
	missing := &os.LinkError{Op: "renameat", Old: "missing.json.tmp", New: "missing.json", Err: os.ErrNotExist}
	saveRename = func(*os.Root, string, string) error {
		calls++
		return missing
	}
	saveSleep = func(time.Duration) { t.Error("unexpected retry sleep") }
	if err := SaveResult(Result{ID: "missing"}); err != missing {
		t.Errorf("save error = %v, want %v", err, missing)
	}
	if calls != 1 {
		t.Errorf("rename calls = %d, want 1", calls)
	}
	if _, err := os.Stat(filepath.Join(batchesDir(), "missing.json.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temporary file stat = %v, want not exist", err)
	}
}
