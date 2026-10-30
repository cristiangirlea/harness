package runpaths

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Home is $HRN_HOME, or ~/.hrn; an unavailable home yields an unusable path.
func Home() string {
	h, err := ResolveHome()
	if err == nil {
		return h
	}
	// Invalid path characters prevent legacy consumers from writing relative files.
	invalid := "\x00"
	if runtime.GOOS == "windows" {
		invalid = "|"
	}
	h, err = filepath.Abs(invalid)
	if err != nil {
		if runtime.GOOS == "windows" {
			return `C:\|`
		}
		return "/\x00"
	}
	return h
}

// ResolveHome returns the absolute record home or an error.
func ResolveHome() (string, error) {
	if h := os.Getenv("HRN_HOME"); h != "" {
		return filepath.Abs(h)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("cannot resolve home directory: set HRN_HOME")
	}
	return filepath.Abs(filepath.Join(home, ".hrn"))
}

// Root is the runs directory under Home.
func Root() string { return filepath.Join(Home(), "runs") }
