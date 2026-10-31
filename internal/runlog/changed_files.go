package runlog

import (
	"context"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/gitenv"
)

const maxChangedFiles = 500

func gitOutput(dir string, args ...string) (string, error) {
	return gitOutputWithEnv(dir, nil, args...)
}

func gitOutputWithEnv(dir string, extra []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.fsmonitor=false", "-c", "diff.ignoreSubmodules=all", "-c", "core.hooksPath=" + os.DevNull}, args...)...)
	cmd.Env = append(gitenv.WithoutRepoVars(os.Environ()), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1")
	cmd.Env = append(cmd.Env, extra...)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	return string(out), err
}

func startGit(dir string) (string, string) {
	if dir == "" || !gitenv.SupportsNoLazyFetch() {
		return "", ""
	}
	root, err := gitOutput(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", ""
	}
	root = strings.TrimSpace(root)
	head, err := gitOutput(root, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", ""
	}
	return root, strings.TrimSpace(head)
}

func changedFiles(root, head string) ([]string, bool, string, []string) {
	if !gitenv.SupportsNoLazyFetch() {
		return nil, false, "", nil
	}
	off, err := gitenv.FiltersOff(root)
	if err != nil {
		return nil, false, "", nil
	}
	diff, err := gitOutputWithEnv(root, off, "diff", "--no-ext-diff", "--no-textconv", "--ignore-submodules=all", "--no-renames", "--name-only", "-z", head, "--")
	if err != nil {
		return nil, false, "", nil
	}
	untracked, err := gitOutputWithEnv(root, off, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, false, "", nil
	}
	end, err := gitOutput(root, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return nil, false, "", nil
	}
	set := map[string]bool{}
	for _, file := range strings.Split(diff+untracked, "\x00") {
		if file != "" {
			set[file] = true
		}
	}
	files := make([]string, 0, len(set))
	for file := range set {
		files = append(files, file)
	}
	sort.Strings(files)
	truncated := len(files) > maxChangedFiles
	if truncated {
		files = files[:maxChangedFiles]
	}
	end = strings.TrimSpace(end)
	commits, _ := gitOutput(root, "rev-list", "--first-parent", "--max-count=500", head+".."+end, "--")
	return files, truncated, end, strings.Fields(commits)
}
