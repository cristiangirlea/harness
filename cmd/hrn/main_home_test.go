package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A temporary HRN_HOME, so no test reads the owner's records or backends.json
// (#379); a test may still set its own with t.Setenv. A helper process, the
// test binary standing in for another program, keeps what it was started with.
func TestMain(m *testing.M) {
	helper := slices.ContainsFunc(os.Args, func(a string) bool { return strings.HasPrefix(a, "-test.run=^TestHelper") })
	if helper {
		os.Exit(m.Run())
	}
	home, err := os.MkdirTemp("", "hrn-test-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("HRN_HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// The tests never read or write the owner's ~/.hrn: backends.json there could
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
