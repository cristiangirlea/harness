package gc

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/ws"
)

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
