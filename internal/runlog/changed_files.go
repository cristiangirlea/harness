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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.fsmonitor=false"}, args...)...)
	cmd.Env = append(gitenv.WithoutRepoVars(os.Environ()), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	return string(out), err
}

func startGit(dir string) (string, string) {
	if dir == "" {
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

func changedFiles(root, head string) ([]string, bool, string) {
	diff, err := gitOutput(root, "diff", "--no-ext-diff", "--no-renames", "--name-only", "-z", head, "--")
	if err != nil {
		return nil, false, ""
	}
	untracked, err := gitOutput(root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, false, ""
	}
	end, err := gitOutput(root, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return nil, false, ""
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
	return files, truncated, strings.TrimSpace(end)
}
