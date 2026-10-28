package gc

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/ws"
)

func TestHelperHoldGCLockProcess(t *testing.T) {
	if os.Getenv("HRN_GC_HELPER") != "hold" {
		return
	}
	var b [1]byte
	_, _ = os.Stdin.Read(b[:])
}

func TestLiveProcessMetadataKeepsTheLockWhenGuardIsFree(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldGCLockProcess$")
	cmd.Env = append(os.Environ(), "HRN_GC_HELPER=hold")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { stdin.Close(); cmd.Wait() }()
	start, ok := ws.StartID(cmd.Process.Pid)
	if !ok {
		t.Fatal("cannot read the running helper's start identity")
	}
	b, err := json.Marshal(lockInfo{PID: cmd.Process.Pid, Start: start, Time: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "gc.lock"), b, 0600); err != nil {
		t.Fatal(err)
	}
	unlock, err := Lock(time.Now())
	if unlock != nil {
		unlock()
	}
	var held *LockHeldError
	if !errors.As(err, &held) {
		t.Fatalf("expected LockHeldError, got %v", err)
	}
}

func TestLockReclaimsItsOwnStaleMetadata(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	start, ok := ws.StartID(os.Getpid())
	if !ok {
		t.Skip("process start identity unavailable")
	}
	b, _ := json.Marshal(lockInfo{PID: os.Getpid(), Start: start, Time: time.Now()})
	if err := os.WriteFile(filepath.Join(home, "gc.lock"), b, 0600); err != nil {
		t.Fatal(err)
	}
	unlock, err := Lock(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

func TestLockReleaseRetriesRemoval(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	unlock, err := Lock(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	old := removeLock
	defer func() { removeLock = old }()
	calls := 0
	removeLock = func(path string) error {
		calls++
		if calls < 3 {
			return os.ErrPermission
		}
		return os.Remove(path)
	}
	unlock()
	if calls != 3 {
		t.Fatalf("remove called %d times", calls)
	}
	if _, err := os.Stat(filepath.Join(home, "gc.lock")); !os.IsNotExist(err) {
		t.Fatalf("lock remains: %v", err)
	}
}

func TestLockReleaseWhileReaderIsOpen(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	unlock, err := Lock(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	f, err := openLockForRead(filepath.Join(home, "gc.lock"))
	if err != nil {
		unlock()
		t.Fatal(err)
	}
	defer f.Close()
	unlock()
	if _, err := os.Stat(filepath.Join(home, "gc.lock")); !os.IsNotExist(err) {
		t.Fatalf("lock remains: %v", err)
	}
}

func TestScheduleFileCanBeReadAndReplaced(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	path := filepath.Join(home, "gc-last")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := readSharedFile(path)
	if err != nil || string(data) != "old" {
		t.Fatalf("read %q: %v", data, err)
	}
	if err := writeAtomic(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	data, err = readSharedFile(path)
	if err != nil || string(data) != "new" {
		t.Fatalf("read %q: %v", data, err)
	}
}

func TestOldLockIsNotTakenOverWhileGuardIsHeld(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	unlock, err := Lock(time.Now().Add(-7 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	_, err = Lock(time.Now())
	var held *LockHeldError
	if !errors.As(err, &held) {
		t.Fatalf("expected LockHeldError, got %v", err)
	}
}

func TestPostMergePassLogsMainCheckoutFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	root := t.TempDir()
	b, _ := json.Marshal(map[string]any{"enabled": true, "roots": []string{root}})
	os.WriteFile(filepath.Join(home, "gc.json"), b, 0600)
	old := mainCheckoutForPass
	mainCheckoutForPass = func(context.Context, string) (string, bool) { return "", false }
	defer func() { mainCheckoutForPass = old }()
	if ran, err := RunAutomatic(context.Background(), time.Now(), root); !ran || err == nil || !strings.Contains(err.Error(), "main checkout") {
		t.Fatalf("pass %v: %v", ran, err)
	}
	p, err := LastPass()
	if err != nil || !strings.Contains(p.Error, "main checkout") || len(p.Actions) != 0 {
		t.Fatalf("log %+v: %v", p, err)
	}
}

func TestFailedPlannedLogDoesNotAdvanceTheSchedule(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	root := t.TempDir()
	os.WriteFile(filepath.Join(home, "gc.json"), []byte(`{"enabled":true,"roots":[`+strconv.Quote(root)+`]}`), 0600)
	old := appendAutomaticPass
	appendAutomaticPass = func(Pass) error { return errors.New("log failed") }
	defer func() { appendAutomaticPass = old }()
	if _, err := RunAutomatic(context.Background(), time.Now(), ""); err == nil {
		t.Fatal("missing error")
	}
	if _, err := os.Stat(filepath.Join(home, "gc-last")); !os.IsNotExist(err) {
		t.Fatalf("schedule advanced: %v", err)
	}
}

func TestPostMergePassFindsMainCheckoutFromLinkedWorktree(t *testing.T) {
	f := newFixture(t)
	task, _ := f.worktree(filepath.Join(f.repo, ".hrn", "worktrees", "task"), "task")
	f.land("task")
	linked := filepath.Join(f.root, "linked")
	git(t, f.repo, "worktree", "add", "--detach", linked, "main")
	other := filepath.Join(f.root, "other")
	git(t, f.root, "clone", "-q", f.origin, other)
	otherWT := filepath.Join(f.root, "other-wt")
	git(t, other, "worktree", "add", "--detach", otherWT, "main")
	saveBatch(t, "first", f.repo, batch.TaskResult{ID: "task", Status: batch.StatusPass, Worktree: task})
	saveBatch(t, "second", other, batch.TaskResult{ID: "other", Status: batch.StatusPass, Worktree: otherWT})
	b, _ := json.Marshal(map[string]any{"enabled": true, "roots": []string{f.repo}})
	os.WriteFile(filepath.Join(os.Getenv("HRN_HOME"), "gc.json"), b, 0600)
	if ran, err := RunAutomatic(context.Background(), time.Now(), linked); !ran || err != nil {
		t.Fatalf("pass %v: %v", ran, err)
	}
	if exists(task) || !exists(linked) || !exists(otherWT) {
		t.Fatalf("task %v linked %v other %v", exists(task), exists(linked), exists(otherWT))
	}
}

func TestDeadAndOldLocksAreTakenOverAndLiveLocksReported(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	now := time.Now().UTC()
	for _, l := range []lockInfo{{PID: 99999999, Time: now}, {PID: os.Getpid(), Time: now.Add(-7 * time.Hour)}} {
		b, _ := json.Marshal(l)
		os.WriteFile(filepath.Join(home, "gc.lock"), b, 0600)
		unlock, err := Lock(now)
		if err != nil {
			t.Fatal(err)
		}
		unlock()
		p, err := LastPass()
		if err != nil || !strings.Contains(p.Error, "took over") {
			t.Fatalf("takeover %+v %v", p, err)
		}
	}
	start, _ := ws.StartID(os.Getpid())
	b, _ := json.Marshal(lockInfo{PID: os.Getpid(), Start: start, Time: now.Add(-time.Hour)})
	os.WriteFile(filepath.Join(home, "gc.lock"), b, 0600)
	guard, ok, err := ws.TryLock(filepath.Join(home, "gc.guard"))
	if err != nil || !ok {
		t.Fatalf("guard: %v", err)
	}
	defer guard()
	if _, err := Lock(now); err == nil || !strings.Contains(err.Error(), "gc.lock held since") {
		t.Fatalf("held: %v", err)
	}
}

func TestAutomaticPassReportsALockHeldLongerThanTheInterval(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	root := t.TempDir()
	now := time.Now()
	os.WriteFile(filepath.Join(home, "gc.json"), []byte(`{"enabled":true,"roots":[`+strconv.Quote(root)+`],"interval":"1h"}`), 0600)
	unlock, err := Lock(now.Add(-2 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if ran, err := RunAutomatic(context.Background(), now, ""); ran || err == nil || !strings.Contains(err.Error(), "gc.lock held since") {
		t.Fatalf("ran %v: %v", ran, err)
	}
	if warning := LockWarning(now); !strings.Contains(warning, "gc.lock held since") {
		t.Fatalf("warning %q", warning)
	}
	if ran, err := RunAutomatic(context.Background(), now.Add(-time.Hour), root); ran || err == nil || !strings.Contains(err.Error(), "gc.lock held since") {
		t.Fatalf("post-merge ran %v: %v", ran, err)
	}
}

func TestPlannedActionsAreLoggedBeforeCacheRemoval(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	root := tempRoot(t)
	cache := tagged(t, filepath.Join(root, "target"), 30*time.Hour, 100)
	os.WriteFile(filepath.Join(home, "gc.json"), []byte(`{"enabled":true,"roots":[`+strconv.Quote(root)+`]}`), 0600)
	old := removeCacheEntry
	defer func() { removeCacheEntry = old }()
	seen := false
	removeCacheEntry = func(path string) error {
		p, err := LastPass()
		if err != nil || !p.Started || len(p.Planned) != 1 || samePath(p.Planned[0].Path) != samePath(cache) {
			t.Errorf("planned: %+v %v", p, err)
		}
		seen = true
		return old(path)
	}
	if _, err := RunAutomatic(context.Background(), time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("removal was not reached")
	}
}

func TestCorruptLastPassFailsClosed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	root := t.TempDir()
	os.WriteFile(filepath.Join(home, "gc.json"), []byte(`{"enabled":true,"roots":[`+strconv.Quote(root)+`]}`), 0600)
	os.WriteFile(filepath.Join(home, "gc-last"), []byte("bad"), 0600)
	if _, err := RunAutomatic(context.Background(), time.Now(), ""); err == nil {
		t.Fatal("corrupt timestamp accepted")
	}
}

func TestConfigRejectsUnsafeRootsAndInvalidOptions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	root := t.TempDir()
	for _, value := range []string{`{`, `{"enabled":true}`, `{"enabled":true,"roots":["relative"]}`, `{"enabled":true,"roots":[` + strconv.Quote(filepath.VolumeName(root)+string(filepath.Separator)) + `]}`, `{"enabled":true,"roots":[` + strconv.Quote(filepath.Join(root, "missing")) + `]}`, `{"enabled":true,"roots":[` + strconv.Quote(root) + `],"interval":"-1h"}`, `{"enabled":true,"roots":[` + strconv.Quote(root) + `],"interval":"bad"}`, `{"enabled":true,"roots":[` + strconv.Quote(root) + `],"free_below_bytes":-1}`} {
		if err := os.WriteFile(filepath.Join(home, "gc.json"), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(); err == nil {
			t.Errorf("accepted %s", value)
		}
	}
}

func TestFutureLastPassIsDue(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	root := t.TempDir()
	now := time.Now().UTC()
	os.WriteFile(filepath.Join(home, "gc.json"), []byte(`{"enabled":true,"roots":[`+strconv.Quote(root)+`]}`), 0600)
	os.WriteFile(filepath.Join(home, "gc-last"), []byte(now.Add(time.Hour).Format(time.RFC3339Nano)), 0600)
	if ran, err := RunAutomatic(context.Background(), now, ""); !ran || err != nil {
		t.Fatalf("ran %v err %v", ran, err)
	}
}
