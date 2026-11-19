//go:build !windows

package prefs

import (
	"io/fs"
	"os"
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

func makeOrgUnreadable(t *testing.T, path string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root can read files with mode 000")
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
}
