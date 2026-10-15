package update

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"
)

// TestHelperVersion is the fake hrn's `version` subcommand: the test binary,
// re-run with this test selected, prints the line the test put in the
// environment. Copies of the test binary stand in for installed hrn copies.
func TestHelperVersion(t *testing.T) {
	line := os.Getenv("HRN_UPDATE_HELPER_VERSION")
	if line == "" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			if len(os.Args) > i+1 && os.Args[i+1] == "version" {
				fmt.Println(line)
				os.Exit(0)
			}
			fmt.Fprintln(os.Stderr, "the candidate may only be run with version")
			os.Exit(3)
		}
	}
	os.Exit(99)
}

// TestHelperSleep keeps a copy of the test binary running until its stdin
// closes, so a test can swap a file that a process is executing. With
// `-- sleep` (a detached child has no stdin pipe) it sleeps for a while.
func TestHelperSleep(t *testing.T) {
	if os.Getenv("HRN_UPDATE_HELPER_SLEEP") != "1" {
		return
	}
	fmt.Println("sleeping")
	if i := slices.Index(os.Args, "--"); i >= 0 && i+1 < len(os.Args) && os.Args[i+1] == "sleep" {
		time.Sleep(30 * time.Second)
	} else {
		io.Copy(io.Discard, os.Stdin)
	}
	os.Exit(0)
}

// fakeHrn writes a copy of the test binary to path, with marker appended so
// that copies differ in size; the size is how fakeVersion tells them apart.
func fakeHrn(t *testing.T, path string, marker string) {
	t.Helper()
	src, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(src, []byte(marker)...), 0o755); err != nil {
		t.Fatal(err)
	}
}

// fakeVersion answers `version` for the fake binaries by running them the
// way the real one is run, with the line for their size in the environment.
// A path with no line is "not hrn": the helper prints nothing and the parse
// fails, as it would for git or any other program.
func fakeVersion(t *testing.T, lines map[string]string) VersionFunc {
	t.Helper()
	bySize := map[int64]string{}
	for path, line := range lines {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		bySize[info.Size()] = line
	}
	return func(ctx context.Context, path string) (Version, error) {
		info, err := os.Stat(path)
		if err != nil {
			return Version{}, err
		}
		line := bySize[info.Size()]
		cmd := exec.CommandContext(ctx, path, "-test.run=^TestHelperVersion$", "--", "version")
		cmd.Env = append(os.Environ(), "HRN_UPDATE_HELPER_VERSION="+line)
		out, err := cmd.Output()
		if err != nil && line != "" {
			return Version{}, fmt.Errorf("%s version: %w", path, err)
		}
		return ParseVersionLine(string(out))
	}
}

func exe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	x, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	y, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(x) == string(y)
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
