package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A temporary HRN_HOME, so no test reads the user's records or backends.json
// (#379); a test may still set its own with t.Setenv. A helper process, the
// test binary standing in for another program, keeps what it was started with.
func TestMain(m *testing.M) {
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(os.Args[0])), ".exe")
	if slices.Contains([]string{"claude", "codex", "gemini", "opencode", "agy"}, name) {
		log := os.Getenv("HRN_ZERO_PROVIDER_SHIM_LOG")
		if log == "" {
			os.Exit(1)
		}
		f, err := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(2)
		}
		_, _ = f.WriteString(filepath.Base(os.Args[0]) + "\n")
		_ = f.Close()
		os.Exit(1)
	}
	helper := slices.ContainsFunc(os.Args, func(a string) bool { return strings.HasPrefix(a, "-test.run=^TestHelper") })
	if helper {
		os.Exit(m.Run())
	}
	home, err := os.MkdirTemp("", "hrn-test-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("HRN_HOME", home)
	// The Backends card and hrn backends read codex's sessions for its quota.
	os.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// The tests never read or write the real ~/.hrn: backends.json there could
// disable the backend a test names (#379).
func TestTheTestsRunAgainstATemporaryHRNHome(t *testing.T) {
	home := os.Getenv("HRN_HOME")
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(home)
	if home == "" || err != nil || !strings.HasPrefix(got, tmp) {
		t.Fatalf("HRN_HOME = %q, want a directory under %s", home, tmp)
	}
}
