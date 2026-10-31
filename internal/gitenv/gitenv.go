// Package gitenv confines Git to its directory's repo.
package gitenv

import (
	"context"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

var versionOnce sync.Once
var noLazyFetch bool

// SupportsNoLazyFetch reports whether Git supports disabling lazy fetches.
func SupportsNoLazyFetch() bool {
	versionOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "git", "--version").Output()
		if err == nil {
			noLazyFetch = supportsNoLazyFetch(string(out))
		}
	})
	return noLazyFetch
}

func supportsNoLazyFetch(version string) bool {
	fields := strings.Fields(version)
	if len(fields) < 3 || fields[0] != "git" || fields[1] != "version" {
		return false
	}
	parts := strings.Split(fields[2], ".")
	if len(parts) < 2 {
		return false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	return err1 == nil && err2 == nil && (major > 2 || major == 2 && minor >= 44)
}

// Strip hook overrides to honor the requested directory.
var repoVars = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
	"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE",
}

// WithoutRepoVars strips overrides, ignoring case.
func WithoutRepoVars(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(repoVars, strings.ToUpper(k)) {
			out = append(out, kv)
		}
	}
	return out
}
