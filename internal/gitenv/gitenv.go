// Package gitenv confines Git to its directory's repo.
package gitenv

import (
	"slices"
	"strings"
)

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
