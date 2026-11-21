package plugin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFindPrefersTheInstalledPluginToOneOnPath(t *testing.T) {
	home := pluginHome(t)
	exe := installFake(t, home, "echo")
	pathDir := t.TempDir()
	copyFake(t, pathDir, "echo")
	prependPath(t, pathDir)
	found, err := Find("echo")
	if err != nil {
		t.Fatal(err)
	}
	assertFound(t, found, "echo", exe, true)
}

func TestFindReturnsAPluginFromPathWhenItIsNotInstalled(t *testing.T) {
	pluginHome(t)
	pathDir := t.TempDir()
	exe := copyFake(t, pathDir, "echo")
	prependPath(t, pathDir)
	found, err := Find("echo")
	if err != nil {
		t.Fatal(err)
	}
	assertFound(t, found, "echo", exe, false)
}

func TestFindReportsAMissingPluginByNameAndLeavesHintsToTheCaller(t *testing.T) {
	pluginHome(t)
	_, err := Find("missing-plugin")
	if !errors.Is(err, ErrNotFound) || err.Error() != "plugin not found: missing-plugin" {
		t.Fatalf("Find() error = %v", err)
	}
}

func TestFindRejectsInvalidNamesAsCallerErrors(t *testing.T) {
	pluginHome(t)
	for _, name := range []string{"../x", "A", "", strings.Repeat("a", 65)} {
		if _, err := Find(name); err == nil || errors.Is(err, ErrNotFound) || err.Error() != fmt.Sprintf("plugin: invalid name %q", name) {
			t.Errorf("Find(%q) error = %v", name, err)
		}
	}
}

func TestDiscoveryIgnoresCommandScriptsOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows enables command scripts through PATHEXT")
	}
	pluginHome(t)
	t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hrn-x.cmd"), []byte("@echo off\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prependPath(t, dir)
	if found, err := Find("x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Find() = %+v, %v", found, err)
	}
	if found, err := List(); err != nil || len(found) != 0 {
		t.Errorf("List() = %+v, %v", found, err)
	}
}

func TestFindAndListAgreeWhenAnEarlierPathEntryHoldsALookalike(t *testing.T) {
	pluginHome(t)
	t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
	real := t.TempDir()
	exe := copyFake(t, real, "echo")
	prependPath(t, real)
	lookalike := t.TempDir()
	if err := os.WriteFile(filepath.Join(lookalike, "hrn-echo.exe.bat"), []byte("@echo off\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prependPath(t, lookalike)
	found, err := Find("echo")
	if err != nil {
		t.Fatal(err)
	}
	assertFound(t, found, "echo", exe, false)
	listed, err := List()
	if err != nil || len(listed) != 1 {
		t.Fatalf("List() = %+v, %v", listed, err)
	}
	assertFound(t, listed[0], "echo", exe, false)
}

func TestDiscoveryIgnoresFilesWithoutAnExecuteBitOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not use Unix execute bits")
	}
	home := pluginHome(t)
	dir := t.TempDir()
	prependPath(t, dir)
	installed := filepath.Join(home, "plugins", "echo", "hrn-echo")
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(dir, "hrn-notes"), installed} {
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"notes", "echo"} {
		if found, err := Find(name); !errors.Is(err, ErrNotFound) {
			t.Errorf("Find(%q) = %+v, %v", name, found, err)
		}
	}
	if found, err := List(); err != nil || len(found) != 0 {
		t.Errorf("List() = %+v, %v", found, err)
	}
}

func TestDiscoveryReservesTheNamesOfHrnsOwnPrograms(t *testing.T) {
	home := pluginHome(t)
	installFake(t, home, "server")
	dir := t.TempDir()
	prependPath(t, dir)
	for _, name := range []string{"server", "worker", "fixture-model"} {
		if name != "server" {
			copyFake(t, dir, name)
		}
		if _, err := Find(name); err == nil || errors.Is(err, ErrNotFound) || err.Error() != fmt.Sprintf("plugin: %q is reserved for one of hrn's own programs", name) {
			t.Errorf("Find(%q) error = %v", name, err)
		}
	}
	if found, err := List(); err != nil || len(found) != 0 {
		t.Errorf("List() = %+v, %v", found, err)
	}
}

func TestListSortsPluginsAndKeepsTheFirstLocationForEachName(t *testing.T) {
	home := pluginHome(t)
	first, second := t.TempDir(), t.TempDir()
	installed := installFake(t, home, "echo")
	installFake(t, home, ".staging-123")
	copyFake(t, first, "echo")
	stubborn := copyFake(t, first, "stubborn")
	copyFake(t, second, "stubborn")
	garbage := copyFake(t, second, "garbage")
	prependPath(t, first, second)
	found, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 3 {
		t.Fatalf("List() = %+v", found)
	}
	assertFound(t, found[0], "echo", installed, true)
	assertFound(t, found[1], "garbage", garbage, false)
	assertFound(t, found[2], "stubborn", stubborn, false)
}

func TestListIsEmptyWithoutInstalledOrPathPlugins(t *testing.T) {
	pluginHome(t)
	found, err := List()
	if err != nil || len(found) != 0 {
		t.Fatalf("List() = %+v, %v", found, err)
	}
}

func TestDiscoveryIgnoresRelativePathEntries(t *testing.T) {
	pluginHome(t)
	dir := t.TempDir()
	copyFake(t, filepath.Join(dir, "bin"), "echo")
	t.Chdir(dir)
	prependPath(t, "bin")
	if _, err := Find("echo"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Find() error = %v", err)
	}
	if found, err := List(); err != nil || len(found) != 0 {
		t.Errorf("List() = %+v, %v", found, err)
	}
}

func prependPath(t *testing.T, dirs ...string) {
	t.Helper()
	if path := os.Getenv("PATH"); path != "" {
		dirs = append(dirs, path)
	}
	t.Setenv("PATH", strings.Join(dirs, string(os.PathListSeparator)))
}

func assertFound(t *testing.T, got Found, name, path string, installed bool) {
	t.Helper()
	if got.Name != name || got.Installed != installed || !filepath.IsAbs(got.Path) {
		t.Fatalf("found %+v, want %q at an absolute path with installed=%t", got, name, installed)
	}
	assertSamePath(t, got.Path, path)
}

func assertSamePath(t *testing.T, got, want string) {
	t.Helper()
	got, gotErr := filepath.EvalSymlinks(got)
	want, wantErr := filepath.EvalSymlinks(want)
	if gotErr != nil || wantErr != nil {
		t.Fatalf("resolving paths: got error %v, want error %v", gotErr, wantErr)
	}
	if got != want && !(runtime.GOOS == "windows" && strings.EqualFold(got, want)) {
		t.Fatalf("path = %q, want %q", got, want)
	}
}
