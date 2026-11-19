//go:build windows

package prefs

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestOrgPathIgnoresStartupEnvironment(t *testing.T) {
	const expected = "HRN_TEST_EXPECTED_ORG_PATH"
	if want := os.Getenv(expected); want != "" {
		if got, err := OrgPath(); err != nil || got != want {
			t.Fatalf("organisation path = %q, want %q, error = %v", got, want, err)
		}
		return
	}
	want, err := OrgPath()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		env  []string
	}{
		{"absent", nil},
		{"overridden", []string{"ProgramData=" + dir, "ALLUSERSPROFILE=" + dir, "SystemDrive=" + dir}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestOrgPathIgnoresStartupEnvironment$")
			cmd.Env = append([]string{expected + "=" + want, "SYSTEMROOT=" + os.Getenv("SYSTEMROOT"), "WINDIR=" + os.Getenv("WINDIR")}, tc.env...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("fresh process: %v\n%s", err, out)
			}
		})
	}
}
