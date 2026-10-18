package eval

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/cristiangirlea/harness/internal/batch"
)

// blindCheckout copies base and then commit into a fresh repository under a
// neutral temporary name, as commits "base" and "candidate" on branch candidate:
// the cell's worktree path, branch and commit message all name the variant.
func blindCheckout(ctx context.Context, repo, base, commit string) (_ string, err error) {
	dir, err := os.MkdirTemp("", "hrn-judge-")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			if rerr := os.RemoveAll(dir); rerr != nil {
				err = errors.Join(err, fmt.Errorf("leaving %s behind: %w", dir, rerr))
			}
		}
	}()
	hooks, err := os.MkdirTemp("", "hrn-no-hooks-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(hooks)
	// No template, hooks, fsmonitor or signing: the files are an agent's.
	fresh := []string{"-c", "core.hooksPath=" + hooks, "-c", "core.fsmonitor=false",
		"-c", "commit.gpgSign=false", "-c", "gc.auto=0", "-c", "maintenance.auto=false"}
	run := func(args ...string) error {
		_, err := blindGit(ctx, dir, append(slices.Clone(fresh), args...)...)
		return err
	}
	if err := run("-c", "init.defaultBranch=candidate", "init", "-q", "--template="); err != nil {
		return "", err
	}
	// The judge's own git status must not start a daemon that keeps the copy open.
	if err := run("config", "core.fsmonitor", "false"); err != nil {
		return "", err
	}
	for i, step := range []struct{ commit, message string }{{base, "base"}, {commit, "candidate"}} {
		if i > 0 {
			if err := run("rm", "-rqf", "--ignore-unmatch", "--", "."); err != nil {
				return "", err
			}
		}
		if err := batch.CheckoutTree(ctx, repo, step.commit, dir); err != nil {
			return "", err
		}
		if err := run("add", "-A", "-f"); err != nil {
			return "", err
		}
		if err := run("commit", "-q", "--no-verify", "--allow-empty", "-m", step.message); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// blindEnv drops what would point git at another repository (a git hook that
// starts hrn sets some) and names a neutral author.
func blindEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(name) {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
			"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE":
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GIT_AUTHOR_NAME=judge", "GIT_AUTHOR_EMAIL=judge@localhost",
		"GIT_COMMITTER_NAME=judge", "GIT_COMMITTER_EMAIL=judge@localhost", "LC_ALL=C", "LANGUAGE=")
}

func blindGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = blindEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// judgeOrder is a shuffle of 0..n-1 seeded by the eval, so verdicts are not
// asked for variant by variant and the same eval asks in the same order.
func judgeOrder(n int, seed string) []int {
	h := fnv.New64a()
	h.Write([]byte(seed))
	s := h.Sum64()
	return rand.New(rand.NewPCG(s, s)).Perm(n)
}
