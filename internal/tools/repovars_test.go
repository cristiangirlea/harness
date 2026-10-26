package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAShellCommandActsOnItsOwnDirectoryWhateverGitDirHrnInherited(t *testing.T) {
	own, other := t.TempDir(), t.TempDir()
	for _, dir := range []string{own, other} {
		if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v\n%s", err, out)
		}
	}
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	for _, env := range [][]string{nil, os.Environ()} {
		res, err := RunShell(context.Background(), own, "git rev-parse --show-toplevel", 0, env)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := filepath.EvalSymlinks(strings.TrimSpace(res.Output))
		want, _ := filepath.EvalSymlinks(own)
		if !strings.EqualFold(filepath.Clean(got), filepath.Clean(want)) {
			t.Errorf("explicit env %v: git saw %q, want %s", env != nil, res.Output, want)
		}
	}
}
