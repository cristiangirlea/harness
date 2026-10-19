// Package gitenv keeps git run by hrn on the repository of the directory it runs in.
package gitenv

import (
	"slices"
	"strings"
)

// repoVars point git at a repository, work tree, index or object store other
// than the one its directory holds; a git hook that starts hrn sets some.
var repoVars = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
	"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE",
}

// WithoutRepoVars returns env without repoVars, any case.
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
