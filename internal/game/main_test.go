package game

import (
	"os"
	"testing"

	"github.com/cristiangirlea/harness/internal/scripthint"
)

// TestMain moves the temp dir out of any git checkout, since folders under one would take its repository name.
func TestMain(m *testing.M) {
	os.Exit(runOutsideRepo(m))
}

func runOutsideRepo(m *testing.M) int {
	if _, inside := scripthint.Root(os.TempDir()); !inside {
		return m.Run()
	}
	var bases []string
	if dir, err := os.UserCacheDir(); err == nil {
		bases = append(bases, dir)
	}
	if dir, err := os.UserHomeDir(); err == nil {
		bases = append(bases, dir)
	}
	for _, base := range bases {
		if _, inside := scripthint.Root(base); inside {
			continue
		}
		dir, err := os.MkdirTemp(base, "hrn-game-test-")
		if err != nil {
			continue
		}
		defer os.RemoveAll(dir)
		for _, name := range []string{"TMP", "TEMP", "TMPDIR"} {
			os.Setenv(name, dir)
		}
		return m.Run()
	}
	return m.Run()
}
