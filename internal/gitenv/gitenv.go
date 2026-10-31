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

type versionCache struct {
	mu               sync.Mutex
	checked          bool
	supportedVersion bool
	retryAfter       time.Time
	check            func() (string, error)
	now              func() time.Time
}

var gitVersion = versionCache{
	check: func() (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "git", "--version").Output()
		return string(out), err
	},
	now: time.Now,
}

// SupportsNoLazyFetch reports whether Git supports disabling lazy fetches.
func SupportsNoLazyFetch() bool { return gitVersion.supported() }

func (c *versionCache) supported() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.checked {
		return c.supportedVersion
	}
	now := c.now()
	if now.Before(c.retryAfter) {
		return false
	}
	version, err := c.check()
	supported, valid := parseVersion(version)
	if err != nil || !valid {
		c.retryAfter = c.now().Add(time.Minute)
		return false
	}
	c.checked, c.supportedVersion = true, supported
	return supported
}

func supportsNoLazyFetch(version string) bool {
	supported, _ := parseVersion(version)
	return supported
}

func parseVersion(version string) (bool, bool) {
	fields := strings.Fields(version)
	if len(fields) < 3 || fields[0] != "git" || fields[1] != "version" {
		return false, false
	}
	parts := strings.Split(fields[2], ".")
	if len(parts) < 2 {
		return false, false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || major < 0 || minor < 0 {
		return false, false
	}
	return major > 2 || major == 2 && minor >= 44, true
}

// Strip hook overrides to honor the requested directory.
var repoVars = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
	"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE", "GIT_CONFIG_PARAMETERS",
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
