package gitenv

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWithoutRepoVarsDropsOnlyWhatPointsGitElsewhere(t *testing.T) {
	env := []string{"GIT_DIR=/x/.git", "git_work_tree=/x", "GIT_INDEX_FILE=/x/i", "GIT_COMMON_DIR=/x", "GIT_OBJECT_DIRECTORY=/x/o",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=/y", "GIT_NAMESPACE=n", "GIT_CONFIG_COUNT=1", "git_config_parameters=bad", "GIT_AUTHOR_NAME=a", "PATH=/bin"}
	if got := strings.Join(WithoutRepoVars(env), " "); got != "GIT_CONFIG_COUNT=1 GIT_AUTHOR_NAME=a PATH=/bin" {
		t.Errorf("kept %s", got)
	}
}

func TestSupportsNoLazyFetchVersion(t *testing.T) {
	for _, tt := range []struct {
		version string
		want    bool
	}{
		{"git version 2.43.9", false}, {"git version 2.44.0", true},
		{"git version 2.49.0.windows.1", true}, {"git version 3.0", true}, {"bad", false},
	} {
		if got := supportsNoLazyFetch(tt.version); got != tt.want {
			t.Errorf("%q: got %v", tt.version, got)
		}
	}
}

func TestVersionCacheRetriesFailures(t *testing.T) {
	now := time.Now()
	calls := 0
	c := versionCache{check: func() (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("unavailable")
		}
		if calls == 2 {
			return "malformed", nil
		}
		return "git version 2.49.0", nil
	}, now: func() time.Time { return now }}
	if c.supported() || calls != 1 {
		t.Fatal("first failure")
	}
	if c.supported() || calls != 1 {
		t.Fatal("failure was not rate limited")
	}
	now = now.Add(time.Minute)
	if c.supported() || calls != 2 {
		t.Fatal("malformed version")
	}
	now = now.Add(time.Minute)
	if !c.supported() || calls != 3 {
		t.Fatal("retry did not succeed")
	}
	now = now.Add(time.Minute)
	if !c.supported() || calls != 3 {
		t.Fatal("success was not cached")
	}
}
