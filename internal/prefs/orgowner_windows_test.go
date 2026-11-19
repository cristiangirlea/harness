//go:build windows

package prefs

import (
	"syscall"
	"testing"
)

func TestAdminOrgOwner(t *testing.T) {
	for _, tc := range []struct {
		sid     string
		trusted bool
	}{
		{"S-1-5-32-544", true},
		{"S-1-5-18", true},
		{"S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464", true},
		{"S-1-5-32-545", false},
		{"S-1-5-19", false},
		{"S-1-5-21-1-2-3-500", false},
	} {
		sid, err := syscall.StringToSid(tc.sid)
		if err != nil {
			t.Fatal(err)
		}
		if got := adminOrgOwner(sid); got != tc.trusted {
			t.Errorf("trusted = %v for SID %s", got, tc.sid)
		}
	}
}

func skipTrustedScratchOwner(t *testing.T, path string) {
	t.Helper()
	trusted, err := withOrgOwner(path, adminOrgOwner)
	if err != nil {
		t.Fatal(err)
	}
	if trusted {
		t.Skip("scratch file is owned by an administrator account")
	}
}
