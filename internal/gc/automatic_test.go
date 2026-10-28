package gc

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAutomaticPassIsDisabledUntilConfiguredAndRespectsIntervalAndLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	root := t.TempDir()
	now := time.Now().UTC()
	if ran, err := RunAutomatic(context.Background(), now, ""); err != nil || ran {
		t.Fatalf("disabled: %v %v", ran, err)
	}
	if err := os.WriteFile(filepath.Join(home, "gc.json"), []byte(`{"enabled":true,"roots":[`+strconv.Quote(root)+`],"interval":"24h"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "gc.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if ran, err := RunAutomatic(context.Background(), now, ""); err != nil || ran {
		t.Fatalf("locked: %v %v", ran, err)
	}
	os.Remove(filepath.Join(home, "gc.lock"))
	if ran, err := RunAutomatic(context.Background(), now, ""); err != nil || !ran {
		t.Fatalf("first: %v %v", ran, err)
	}
	if ran, err := RunAutomatic(context.Background(), now.Add(time.Hour), ""); err != nil || ran {
		t.Fatalf("interval: %v %v", ran, err)
	}
	if ran, err := RunAutomatic(context.Background(), now.Add(24*time.Hour), ""); err != nil || !ran {
		t.Fatalf("due: %v %v", ran, err)
	}
	last, err := LastPass()
	if err != nil || !last.Time.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("last: %+v %v", last, err)
	}
}

func TestAutomaticLogRecordsActionsAndIsBounded(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for i := 0; i < 200; i++ {
		p := Pass{Time: time.Now(), Actions: []Action{{Path: strings.Repeat("x", 10000), Bytes: 12, Reason: "idle", Kind: "remove"}}}
		if err := appendPass(p); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(filepath.Join(os.Getenv("HRN_HOME"), "gc.log"))
	if err != nil || info.Size() > maxLogBytes {
		t.Fatalf("log size: %v %v", info, err)
	}
	p, err := LastPass()
	if err != nil || len(p.Actions) != 1 || p.Actions[0].Bytes != 12 || p.Actions[0].Reason != "idle" {
		t.Fatalf("pass: %+v %v", p, err)
	}
}

func TestAutomaticPassLogsRemovedCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	root := tempRoot(t)
	cache := tagged(t, filepath.Join(root, "target"), 30*time.Hour, 100)
	config := `{"enabled":true,"roots":[` + strconv.Quote(root) + `]}`
	if err := os.WriteFile(filepath.Join(home, "gc.json"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if ran, err := RunAutomatic(context.Background(), time.Now(), ""); !ran || err != nil {
		t.Fatalf("run: %v %v", ran, err)
	}
	p, err := LastPass()
	if err != nil || len(p.Actions) != 1 || samePath(p.Actions[0].Path) != samePath(cache) || p.Actions[0].Bytes < 100 || p.Actions[0].Reason == "" || p.Actions[0].Kind != "remove" {
		t.Fatalf("pass: %+v %v", p, err)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatalf("cache still exists: %v", err)
	}
}

func TestAutomaticPassDoesNotRemoveWhenLogIsUnavailable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	root := tempRoot(t)
	cache := tagged(t, filepath.Join(root, "target"), 30*time.Hour, 100)
	config := `{"enabled":true,"roots":[` + strconv.Quote(root) + `]}`
	if err := os.WriteFile(filepath.Join(home, "gc.json"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "gc.log"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := RunAutomatic(context.Background(), time.Now(), ""); err == nil {
		t.Fatal("expected log error")
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("cache was removed: %v", err)
	}
}
