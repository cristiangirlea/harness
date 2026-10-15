package update

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestInstallDirsNamesWherehrnIsPutOnEachSystem(t *testing.T) {
	env := map[string]string{
		"PATH":         strings.Join([]string{"/usr/local/bin", "/home/u/go/bin"}, string(os.PathListSeparator)),
		"GOPATH":       "/gopath",
		"LOCALAPPDATA": `C:\Users\u\AppData\Local`,
	}
	getenv := func(k string) string { return env[k] }
	got := InstallDirs(getenv, "linux", "/home/u")
	for _, want := range []string{"/usr/local/bin", "/home/u/go/bin", filepath.Join("/gopath", "bin")} {
		if !contains(got, want) {
			t.Errorf("linux dirs %v lack %q", got, want)
		}
	}
	if contains(got, filepath.Join(`C:\Users\u\AppData\Local`, "hrn")) {
		t.Errorf("linux dirs %v name the Windows folder", got)
	}
	got = InstallDirs(getenv, "windows", `C:\Users\u`)
	if !contains(got, filepath.Join(`C:\Users\u\AppData\Local`, "hrn")) {
		t.Errorf("windows dirs %v lack %%LOCALAPPDATA%%\\hrn", got)
	}
	// Without GOPATH, Go's default is ~/go.
	delete(env, "GOPATH")
	if got = InstallDirs(getenv, "linux", "/home/u"); !contains(got, filepath.Join("/home/u", "go", "bin")) {
		t.Errorf("without GOPATH %v lacks ~/go/bin", got)
	}
}

func TestFindTargetsStartsWithTheRunningBinaryAndDeduplicates(t *testing.T) {
	root := t.TempDir()
	a, b, c := filepath.Join(root, "a"), filepath.Join(root, "b"), filepath.Join(root, "c")
	for _, d := range []string{a, b, c} {
		os.MkdirAll(d, 0o755)
	}
	name := "hrn"
	if runtime.GOOS == "windows" {
		name = "hrn.exe"
	}
	os.WriteFile(filepath.Join(a, name), []byte("a"), 0o755)
	os.WriteFile(filepath.Join(b, name), []byte("b"), 0o755)
	os.WriteFile(filepath.Join(c, "hrn.txt"), []byte("c"), 0o644)
	os.WriteFile(filepath.Join(c, "hrn-a0e5d2f12345-20261015-0907.prev"), []byte("c"), 0o755)

	// The running copy is a/hrn, reached through a differently spelled path;
	// b is listed twice; c has nothing called hrn.
	running := filepath.Join(a, ".", name)
	got, err := FindTargets(running, []string{b, b, c, filepath.Join(root, "missing"), a})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{mustEval(t, filepath.Join(a, name)), mustEval(t, filepath.Join(b, name))}
	if len(got) != len(want) {
		t.Fatalf("got %v; want %v", got, want)
	}
	for i := range want {
		if !samePath(got[i], want[i]) {
			t.Errorf("[%d] %q; want %q", i, got[i], want[i])
		}
	}
}

func TestFindTargetsOnWindowsIgnoresCaseWhenDeduplicating(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("case-insensitive paths")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "hrn.exe"), []byte("a"), 0o755)
	got, err := FindTargets(filepath.Join(dir, "hrn.exe"), []string{strings.ToUpper(dir)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("got %v; want one target", got)
	}
}

func contains(list []string, s string) bool { return slices.Contains(list, s) }
