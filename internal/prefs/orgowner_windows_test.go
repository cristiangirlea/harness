//go:build windows

package prefs

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func makeOrgUnreadable(t *testing.T, _ string) {
	t.Helper()
	t.Skip("Windows chmod cannot deny read access")
}

func TestLoadOrgRefusesJunction(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "backends.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	junction := filepath.Join(dir, "junction")
	cmd := exec.Command("cmd", "/c", "mklink", "/J", junction, target)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot create junction: %v\n%s", err, out)
	}
	useOrgPath(t, filepath.Join(junction, "backends.json"), true)
	requireOrgError(t, "not owned by an administrator account")
}

func TestLoadOrgReportsSharingViolation(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "backends.json")
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(h)
	// Some accounts open past an exclusive holder; the mapping itself is tested on every system.
	if f, err := os.Open(path); err == nil {
		f.Close()
		t.Skip("this account can open a file another handle holds exclusively")
	}
	useOrgPath(t, path, true)
	requireOrgError(t, "could not be read")
}

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
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := checkOrgFile(syscall.Handle(f.Fd()), false); err != errOrgOwner {
		t.Fatalf("scratch file handle owner error = %v", err)
	}
}
