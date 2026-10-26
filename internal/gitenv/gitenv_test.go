package gitenv

import (
	"strings"
	"testing"
)

func TestWithoutRepoVarsDropsOnlyWhatPointsGitElsewhere(t *testing.T) {
	env := []string{"GIT_DIR=/x/.git", "git_work_tree=/x", "GIT_INDEX_FILE=/x/i", "GIT_COMMON_DIR=/x", "GIT_OBJECT_DIRECTORY=/x/o",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=/y", "GIT_NAMESPACE=n", "GIT_CONFIG_COUNT=1", "GIT_AUTHOR_NAME=a", "PATH=/bin"}
	if got := strings.Join(WithoutRepoVars(env), " "); got != "GIT_CONFIG_COUNT=1 GIT_AUTHOR_NAME=a PATH=/bin" {
		t.Errorf("kept %s", got)
	}
}
