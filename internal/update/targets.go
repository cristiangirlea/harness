package update

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// InstallDirs is where hrn copies may be: every PATH entry, Go's binary
// folder ($GOPATH/bin, else ~/go/bin) and, on Windows, %LOCALAPPDATA%\hrn.
func InstallDirs(getenv func(string) string, goos, home string) []string {
	var dirs []string
	for _, d := range filepath.SplitList(getenv("PATH")) {
		if d != "" {
			dirs = append(dirs, d)
		}
	}
	if gopath := getenv("GOPATH"); gopath != "" {
		for _, g := range filepath.SplitList(gopath) {
			dirs = append(dirs, filepath.Join(g, "bin"))
		}
	} else if home != "" {
		dirs = append(dirs, filepath.Join(home, "go", "bin"))
	}
	if goos == "windows" {
		if local := getenv("LOCALAPPDATA"); local != "" {
			dirs = append(dirs, filepath.Join(local, "hrn"))
		}
	}
	return dirs
}

// FindTargets lists the hrn binaries to update: the running one first, then
// every hrn (hrn.exe on Windows) in dirs, each path resolved through
// EvalSymlinks and listed once.
func FindTargets(running string, dirs []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			return
		}
		info, err := os.Stat(r)
		if err != nil || !info.Mode().IsRegular() {
			return
		}
		key := r
		if runtime.GOOS == "windows" {
			key = strings.ToLower(r)
		}
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, r)
	}
	if running != "" {
		add(running)
	}
	name := "hrn"
	if runtime.GOOS == "windows" {
		name = "hrn.exe"
	}
	for _, d := range dirs {
		add(filepath.Join(d, name))
	}
	return out, nil
}

// samePath compares two resolved paths the way the file system does.
func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
