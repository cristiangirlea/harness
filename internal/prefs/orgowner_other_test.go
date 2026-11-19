//go:build !windows

package prefs

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestAdminOrgOwner(t *testing.T) {
	for _, tc := range []struct {
		uid     uint32
		mode    fs.FileMode
		trusted bool
	}{
		{0, 0644, true},
		{0, fs.ModeDir | 0755, true},
		{0, 0664, false},
		{0, fs.ModeDir | 0757, false},
		{1000, 0600, false},
	} {
		if got := adminOrgOwner(tc.uid, tc.mode); got != tc.trusted {
			t.Errorf("trusted = %v for uid %d and mode %v", got, tc.uid, tc.mode)
		}
	}
}

func skipTrustedScratchOwner(t *testing.T, _ string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("test is running as root")
	}
}

func TestLoadOrgRefusesSymlink(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	old, oldTrusted := orgPathOverride, orgPathTrusted
	t.Cleanup(func() { orgPathOverride, orgPathTrusted = old, oldTrusted })
	orgPathOverride, orgPathTrusted = filepath.Join(dir, "backends.json"), true
	if err := os.Symlink(path, orgPathOverride); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrg(); err == nil || err.Error() != "the organisation's backends.json: not owned by an administrator account" {
		t.Fatalf("symlink policy error = %v", err)
	}
}
